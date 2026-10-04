package exec

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/value"
)

// op is a compiled operator; open may be called many times (once per outer
// row for Map subplans).
type op interface {
	layout() *layout
	open(ctx context.Context, e *env) (stream, error)
}

// stream yields non-empty batches; a nil batch means end of stream.
type stream interface {
	next(ctx context.Context) ([]value.Row, error)
	close()
}

// compile turns an IR node into an operator. outer is the layout visible to
// ir.Outer references (nil outside Map subplans).
func compile(n ir.Node, outer *layout) (op, error) {
	switch n := n.(type) {
	case *ir.Get:
		key, err := compileAll(n.Key, nil, outer)
		if err != nil {
			return nil, err
		}
		return &getOp{rel: n.Rel, path: n.Path, cols: n.Cols, keys: [][]evalFn{key},
			lay: relLayout(n.Rel, ir.AliasOr(n.Alias, n.Rel))}, nil

	case *ir.GetMany:
		g := &getOp{rel: n.Rel, path: n.Path, cols: n.Cols, lay: relLayout(n.Rel, ir.AliasOr(n.Alias, n.Rel))}
		for _, k := range n.Keys {
			key, err := compileAll(k, nil, outer)
			if err != nil {
				return nil, err
			}
			g.keys = append(g.keys, key)
		}
		return g, nil

	case *ir.Scan:
		s := &scanOp{n: n, lay: relLayout(n.Rel, ir.AliasOr(n.Alias, n.Rel))}
		var err error
		if s.eq, err = compileAll(n.Eq, nil, outer); err != nil {
			return nil, err
		}
		if s.lo, err = compileBound(n.Lo, outer); err != nil {
			return nil, err
		}
		if s.hi, err = compileBound(n.Hi, outer); err != nil {
			return nil, err
		}
		return s, nil

	case *ir.Filter:
		in, err := compile(n.Input, outer)
		if err != nil {
			return nil, err
		}
		pred, err := compileExpr(n.Pred, in.layout(), outer)
		if err != nil {
			return nil, err
		}
		return &filterOp{in: in, pred: pred}, nil

	case *ir.Project:
		in, err := compile(n.Input, outer)
		if err != nil {
			return nil, err
		}
		p := &projectOp{in: in, lay: &layout{}}
		for _, ne := range n.Exprs {
			f, err := compileExpr(ne.Expr, in.layout(), outer)
			if err != nil {
				return nil, err
			}
			p.exprs = append(p.exprs, f)
			p.lay.fields = append(p.lay.fields, field{name: ne.Name})
		}
		return p, nil

	case *ir.Lookup:
		in, err := compile(n.Input, outer)
		if err != nil {
			return nil, err
		}
		key, err := compileAll(n.Key, in.layout(), outer)
		if err != nil {
			return nil, err
		}
		return &lookupOp{n: n, in: in, key: key,
			lay: concat(in.layout(), relLayout(n.Rel, ir.AliasOr(n.Alias, n.Rel)))}, nil

	case *ir.Map:
		in, err := compile(n.Input, outer)
		if err != nil {
			return nil, err
		}
		m := &mapOp{in: in}
		extra := &layout{}
		for _, f := range n.Fields {
			sub, err := compile(f.Plan, in.layout())
			if err != nil {
				return nil, fmt.Errorf("exec: field %q: %w", f.Name, err)
			}
			m.fields = append(m.fields, mapField{plan: sub, one: f.One, names: sub.layout().columnNames()})
			extra.fields = append(extra.fields, field{name: f.Name})
		}
		m.lay = concat(in.layout(), extra)
		return m, nil

	case *ir.Limit:
		in, err := compile(n.Input, outer)
		if err != nil {
			return nil, err
		}
		return &limitOp{in: in, n: n.N}, nil

	case *ir.Return:
		return compile(n.Input, outer)
	}
	return nil, fmt.Errorf("exec: cannot execute %T as a read", n)
}

type boundFn struct {
	f         evalFn
	inclusive bool
}

func compileBound(b *ir.Bound, outer *layout) (*boundFn, error) {
	if b == nil {
		return nil, nil
	}
	f, err := compileExpr(b.Expr, nil, outer)
	if err != nil {
		return nil, err
	}
	return &boundFn{f: f, inclusive: b.Inclusive}, nil
}

// coerceKey converts key values to the types of the given key columns.
func coerceKey(rel *schema.Relation, cols []schema.ColID, vals []value.Value) (value.Tuple, error) {
	t := make(value.Tuple, len(vals))
	for i, v := range vals {
		if v.IsNull() {
			continue
		}
		c, err := rel.Columns[cols[i]].Coerce(v)
		if err != nil {
			return nil, err
		}
		t[i] = c
	}
	return t, nil
}

func evalKey(rel *schema.Relation, path *schema.AccessPath, fs []evalFn, row value.Row, e *env) (value.Tuple, error) {
	vals, err := evalAll(fs, row, e)
	if err != nil {
		return nil, err
	}
	return coerceKey(rel, path.KeyCols(), vals)
}

// ---- Get / GetMany ----

type getOp struct {
	rel  *schema.Relation
	path *schema.AccessPath
	cols []schema.ColID
	keys [][]evalFn
	lay  *layout
}

func (g *getOp) layout() *layout { return g.lay }

func (g *getOp) open(ctx context.Context, e *env) (stream, error) {
	keys := make([]value.Tuple, 0, len(g.keys))
	for _, k := range g.keys {
		t, err := evalKey(g.rel, g.path, k, nil, e)
		if err != nil {
			return nil, err
		}
		keys = append(keys, t)
	}
	var rows []value.Row
	if len(keys) == 1 {
		row, ok, err := e.ex.Store.Get(ctx, g.rel, g.path, keys[0], g.cols)
		if err != nil {
			return nil, err
		}
		if ok {
			rows = append(rows, row)
		}
	} else {
		got, err := e.ex.Store.GetMany(ctx, g.rel, g.path, keys, g.cols)
		if err != nil {
			return nil, err
		}
		for _, r := range got {
			if r != nil {
				rows = append(rows, r)
			}
		}
	}
	return &sliceStream{rows: rows}, nil
}

type sliceStream struct{ rows []value.Row }

func (s *sliceStream) next(context.Context) ([]value.Row, error) {
	if len(s.rows) == 0 {
		return nil, nil
	}
	r := s.rows
	s.rows = nil
	return r, nil
}

func (s *sliceStream) close() {}

// ---- Scan ----

type scanOp struct {
	n      *ir.Scan
	eq     []evalFn
	lo, hi *boundFn
	lay    *layout
}

func (s *scanOp) layout() *layout { return s.lay }

func (s *scanOp) open(ctx context.Context, e *env) (stream, error) {
	n := s.n
	eq, err := evalKey(n.Rel, n.Path, s.eq, nil, e)
	if err != nil {
		return nil, err
	}
	req := storage.ScanRequest{Rel: n.Rel, Path: n.Path, Eq: eq, Reverse: n.Reverse, Limit: n.Limit, Cols: n.Cols}
	next := n.Rel.Columns[0]
	if keyCols := n.Path.KeyCols(); len(eq) < len(keyCols) {
		next = n.Rel.Columns[keyCols[len(eq)]]
	}
	if req.Lo, err = evalBound(s.lo, next, e); err != nil {
		return nil, err
	}
	if req.Hi, err = evalBound(s.hi, next, e); err != nil {
		return nil, err
	}
	it, err := e.ex.Store.Scan(ctx, req)
	if err != nil {
		return nil, err
	}
	return &iterStream{it: it, size: e.ex.batchSize()}, nil
}

func evalBound(b *boundFn, col *schema.Column, e *env) (*storage.Bound, error) {
	if b == nil {
		return nil, nil
	}
	v, err := b.f(nil, e)
	if err != nil {
		return nil, err
	}
	if !v.IsNull() {
		if v, err = col.Coerce(v); err != nil {
			return nil, err
		}
	}
	return &storage.Bound{Value: v, Inclusive: b.inclusive}, nil
}

type iterStream struct {
	it   storage.RowIterator
	size int
}

func (s *iterStream) next(ctx context.Context) ([]value.Row, error) {
	var out []value.Row
	for len(out) < s.size && s.it.Next() {
		out = append(out, s.it.Row())
	}
	if err := s.it.Err(); err != nil {
		return nil, err
	}
	return out, ctx.Err()
}

func (s *iterStream) close() { s.it.Close() }

// ---- Filter / Project / Limit ----

type filterOp struct {
	in   op
	pred evalFn
}

func (f *filterOp) layout() *layout { return f.in.layout() }

func (f *filterOp) open(ctx context.Context, e *env) (stream, error) {
	in, err := f.in.open(ctx, e)
	if err != nil {
		return nil, err
	}
	return &mapStream{in: in, fn: func(batch []value.Row) ([]value.Row, error) {
		out := batch[:0:0]
		for _, row := range batch {
			v, err := f.pred(row, e)
			if err != nil {
				return nil, err
			}
			if isTrue(v) {
				out = append(out, row)
			}
		}
		return out, nil
	}}, nil
}

type projectOp struct {
	in    op
	exprs []evalFn
	lay   *layout
}

func (p *projectOp) layout() *layout { return p.lay }

func (p *projectOp) open(ctx context.Context, e *env) (stream, error) {
	in, err := p.in.open(ctx, e)
	if err != nil {
		return nil, err
	}
	return &mapStream{in: in, fn: func(batch []value.Row) ([]value.Row, error) {
		out := make([]value.Row, len(batch))
		for i, row := range batch {
			vals, err := evalAll(p.exprs, row, e)
			if err != nil {
				return nil, err
			}
			out[i] = vals
		}
		return out, nil
	}}, nil
}

// mapStream applies fn to each input batch, skipping batches that become
// empty.
type mapStream struct {
	in stream
	fn func([]value.Row) ([]value.Row, error)
}

func (m *mapStream) next(ctx context.Context) ([]value.Row, error) {
	for {
		b, err := m.in.next(ctx)
		if err != nil || b == nil {
			return nil, err
		}
		out, err := m.fn(b)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 {
			return out, nil
		}
	}
}

func (m *mapStream) close() { m.in.close() }

type limitOp struct {
	in op
	n  int
}

func (l *limitOp) layout() *layout { return l.in.layout() }

func (l *limitOp) open(ctx context.Context, e *env) (stream, error) {
	in, err := l.in.open(ctx, e)
	if err != nil {
		return nil, err
	}
	return &limitStream{in: in, left: l.n}, nil
}

type limitStream struct {
	in   stream
	left int
}

func (l *limitStream) next(ctx context.Context) ([]value.Row, error) {
	if l.left <= 0 {
		return nil, nil
	}
	b, err := l.in.next(ctx)
	if err != nil || b == nil {
		return nil, err
	}
	if len(b) > l.left {
		b = b[:l.left]
	}
	l.left -= len(b)
	return b, nil
}

func (l *limitStream) close() { l.in.close() }

// ---- Lookup ----

type lookupOp struct {
	n   *ir.Lookup
	in  op
	key []evalFn
	lay *layout
}

func (l *lookupOp) layout() *layout { return l.lay }

func (l *lookupOp) open(ctx context.Context, e *env) (stream, error) {
	in, err := l.in.open(ctx, e)
	if err != nil {
		return nil, err
	}
	return &mapStream{in: in, fn: func(batch []value.Row) ([]value.Row, error) {
		return l.process(ctx, e, batch)
	}}, nil
}

// process resolves one input batch. N:1 lookups use a single GetMany per
// batch; 1:N lookups run one bounded scan per row, in parallel.
func (l *lookupOp) process(ctx context.Context, e *env, batch []value.Row) ([]value.Row, error) {
	n := l.n
	keys := make([]value.Tuple, len(batch))
	for i, row := range batch {
		k, err := evalKey(n.Rel, n.Path, l.key, row, e)
		if err != nil {
			return nil, err
		}
		keys[i] = k
	}
	matches := make([][]value.Row, len(batch))
	if !n.Many {
		var idx []int
		var want []value.Tuple
		for i, k := range keys {
			if !k.HasNull() {
				idx = append(idx, i)
				want = append(want, k)
			}
		}
		if len(want) > 0 {
			got, err := e.ex.Store.GetMany(ctx, n.Rel, n.Path, want, n.Cols)
			if err != nil {
				return nil, err
			}
			for j, i := range idx {
				if got[j] != nil {
					matches[i] = []value.Row{got[j]}
				}
			}
		}
	} else {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(e.ex.concurrency())
		for i, k := range keys {
			if k.HasNull() {
				continue
			}
			g.Go(func() error {
				it, err := e.ex.Store.Scan(gctx, storage.ScanRequest{
					Rel: n.Rel, Path: n.Path, Eq: k, Reverse: n.Reverse, Limit: n.Limit, Cols: n.Cols,
				})
				if err != nil {
					return err
				}
				defer it.Close()
				for it.Next() {
					matches[i] = append(matches[i], it.Row())
				}
				return it.Err()
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
	}
	width := len(n.Rel.Columns)
	var out []value.Row
	for i, row := range batch {
		if len(matches[i]) == 0 && n.Optional {
			out = append(out, joinRows(row, make(value.Row, width)))
		}
		for _, m := range matches[i] {
			out = append(out, joinRows(row, m))
		}
	}
	return out, nil
}

func joinRows(a, b value.Row) value.Row {
	out := make(value.Row, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}

// ---- Map ----

type mapField struct {
	plan  op
	one   bool
	names []string
}

type mapOp struct {
	in     op
	fields []mapField
	lay    *layout
}

func (m *mapOp) layout() *layout { return m.lay }

func (m *mapOp) open(ctx context.Context, e *env) (stream, error) {
	in, err := m.in.open(ctx, e)
	if err != nil {
		return nil, err
	}
	return &mapStream{in: in, fn: func(batch []value.Row) ([]value.Row, error) {
		out := make([]value.Row, len(batch))
		for i, row := range batch {
			sub := &env{ex: e.ex, params: e.params, outer: row}
			nested := make(value.Row, len(m.fields))
			for j, f := range m.fields {
				rows, err := run(ctx, f.plan, sub)
				if err != nil {
					return nil, err
				}
				nested[j] = f.nest(rows)
			}
			out[i] = joinRows(row, nested)
		}
		return out, nil
	}}, nil
}

func (f *mapField) nest(rows []value.Row) value.Value {
	if f.one {
		if len(rows) == 0 {
			return value.Null
		}
		return value.NewRecord(f.names, rows[0])
	}
	recs := make([]value.Value, len(rows))
	for i, r := range rows {
		recs[i] = value.NewRecord(f.names, r)
	}
	return value.List(recs)
}
