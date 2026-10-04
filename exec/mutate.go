package exec

import (
	"context"
	"fmt"
	"slices"

	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/value"
)

// mutation collects the changes of one statement before they are applied.
type mutation struct {
	rel     *schema.Relation
	ms      []storage.Mutation
	pending map[string]int // encoded PK -> index in ms
}

func newMutation(rel *schema.Relation) *mutation {
	return &mutation{rel: rel, pending: map[string]int{}}
}

func pkKey(rel *schema.Relation, row value.Row) (string, error) {
	b, err := value.EncodeTuple(nil, rel.PK(row))
	return string(b), err
}

func (m *mutation) add(mu storage.Mutation) error {
	row := mu.New
	if row == nil {
		row = mu.Old
	}
	k, err := pkKey(m.rel, row)
	if err != nil {
		return err
	}
	m.pending[k] = len(m.ms)
	m.ms = append(m.ms, mu)
	return nil
}

// apply writes the batch and evaluates RETURNING over the affected rows.
func (m *mutation) apply(ctx context.Context, e *env, alias string, returning []ir.NamedExpr) (*Result, error) {
	ret, err := compileReturning(m.rel, alias, returning)
	if err != nil {
		return nil, err
	}
	if len(m.ms) > 0 {
		if err := e.ex.Store.Apply(ctx, m.ms); err != nil {
			return nil, err
		}
	}
	res := &Result{RowsAffected: int64(len(m.ms))}
	if ret == nil {
		return res, nil
	}
	res.Columns = ret.lay.columnNames()
	for _, mu := range m.ms {
		row := mu.New
		if row == nil {
			row = mu.Old
		}
		vals, err := evalAll(ret.exprs, row, e)
		if err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, nil
}

type returning struct {
	exprs []evalFn
	lay   *layout
}

func compileReturning(rel *schema.Relation, alias string, es []ir.NamedExpr) (*returning, error) {
	if es == nil {
		return nil, nil
	}
	in := relLayout(rel, alias)
	r := &returning{lay: &layout{}}
	for _, ne := range es {
		f, err := compileExpr(ne.Expr, in, nil)
		if err != nil {
			return nil, err
		}
		r.exprs = append(r.exprs, f)
		r.lay.fields = append(r.lay.fields, field{name: ne.Name})
	}
	return r, nil
}

type assignFn struct {
	col  *schema.Column
	expr evalFn
}

func compileAssigns(rel *schema.Relation, as []ir.Assign, in *layout) ([]assignFn, error) {
	out := make([]assignFn, len(as))
	seen := map[string]bool{}
	for i, a := range as {
		col := rel.Column(a.Col)
		if col == nil {
			return nil, fmt.Errorf("exec: %s has no column %q", rel.Name, a.Col)
		}
		if rel.IsKeyCol(col.ID) {
			return nil, fmt.Errorf("exec: primary key column %s.%s cannot be updated", rel.Name, col.Name)
		}
		if seen[a.Col] {
			return nil, fmt.Errorf("exec: column %q assigned twice", a.Col)
		}
		seen[a.Col] = true
		f, err := compileExpr(a.Expr, in, nil)
		if err != nil {
			return nil, err
		}
		out[i] = assignFn{col: col, expr: f}
	}
	return out, nil
}

// assign evaluates all assignments against src (so every right-hand side sees
// the old values) and applies them to a copy of old.
func assign(as []assignFn, old, src value.Row, e *env) (value.Row, error) {
	vals := make([]value.Value, len(as))
	for i, a := range as {
		v, err := a.expr(src, e)
		if err != nil {
			return nil, err
		}
		if vals[i], err = a.col.Coerce(v); err != nil {
			return nil, err
		}
	}
	row := slices.Clone(old)
	for i, a := range as {
		row[a.col.ID] = vals[i]
	}
	return row, nil
}

func (x *Executor) execInsert(ctx context.Context, n *ir.Insert, e *env) (*Result, error) {
	rel := n.Rel
	alias := ir.AliasOr(n.Alias, rel)
	cols := make([]*schema.Column, 0, len(rel.Columns))
	if n.Cols == nil {
		cols = rel.Columns
	} else {
		for _, name := range n.Cols {
			c := rel.Column(name)
			if c == nil {
				return nil, fmt.Errorf("exec: %s has no column %q", rel.Name, name)
			}
			if slices.Contains(cols, c) {
				return nil, fmt.Errorf("exec: column %q listed twice", name)
			}
			cols = append(cols, c)
		}
	}

	var src []value.Row
	if n.Input != nil {
		in, err := compile(n.Input, nil)
		if err != nil {
			return nil, err
		}
		if w := in.layout().width(); w != len(cols) {
			return nil, fmt.Errorf("exec: insert into %s: input has %d columns, want %d", rel.Name, w, len(cols))
		}
		if src, err = run(ctx, in, e); err != nil {
			return nil, err
		}
	} else {
		for _, r := range n.Rows {
			if len(r) != len(cols) {
				return nil, fmt.Errorf("exec: insert into %s: row has %d values, want %d", rel.Name, len(r), len(cols))
			}
			fs, err := compileAll(r, nil, nil)
			if err != nil {
				return nil, err
			}
			vals, err := evalAll(fs, nil, e)
			if err != nil {
				return nil, err
			}
			src = append(src, vals)
		}
	}

	var set []assignFn
	if c := n.OnConflict; c != nil && !c.DoNothing {
		var err error
		if set, err = compileAssigns(rel, c.Set, concat(relLayout(rel, alias), relLayout(rel, "excluded"))); err != nil {
			return nil, err
		}
	}

	m := newMutation(rel)
	for _, s := range src {
		row := make(value.Row, len(rel.Columns))
		for i, c := range cols {
			row[c.ID] = s[i]
		}
		for i, c := range rel.Columns {
			v, err := c.Coerce(row[i])
			if err != nil {
				return nil, fmt.Errorf("exec: insert into %s: %w", rel.Name, err)
			}
			row[i] = v
		}
		if n.OnConflict == nil {
			if err := m.add(storage.Mutation{Kind: storage.Insert, Rel: rel, New: row}); err != nil {
				return nil, err
			}
			continue
		}
		k, err := pkKey(rel, row)
		if err != nil {
			return nil, err
		}
		if _, dup := m.pending[k]; dup {
			if n.OnConflict.DoNothing {
				continue
			}
			return nil, fmt.Errorf("exec: insert into %s: on-conflict update would affect key %v twice", rel.Name, rel.PK(row))
		}
		existing, found, err := x.Store.Get(ctx, rel, rel.Primary, rel.PK(row), nil)
		if err != nil {
			return nil, err
		}
		switch {
		case !found:
			err = m.add(storage.Mutation{Kind: storage.Insert, Rel: rel, New: row})
		case n.OnConflict.DoNothing:
			continue
		default:
			var upd value.Row
			if upd, err = assign(set, existing, joinRows(existing, row), e); err == nil {
				err = m.add(storage.Mutation{Kind: storage.Update, Rel: rel, Old: existing, New: upd})
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return m.apply(ctx, e, alias, n.Returning)
}

// targetRows runs the read side of an UPDATE or DELETE and extracts the
// target relation's rows (deduplicated by primary key) along with the
// input rows they came from.
func targetRows(ctx context.Context, rel *schema.Relation, alias string, input ir.Node, e *env) (*layout, []value.Row, []value.Row, error) {
	in, err := compile(input, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	lay := in.layout()
	idx := make([]int, len(rel.Columns))
	for i, c := range rel.Columns {
		if idx[i], err = lay.resolve(alias, c.Name); err != nil {
			return nil, nil, nil, fmt.Errorf("exec: input must provide every column of %s as %q: %w", rel.Name, alias, err)
		}
	}
	rows, err := run(ctx, in, e)
	if err != nil {
		return nil, nil, nil, err
	}
	seen := map[string]bool{}
	var targets, srcs []value.Row
	for _, r := range rows {
		t := make(value.Row, len(idx))
		for i, j := range idx {
			t[i] = r[j]
		}
		k, err := pkKey(rel, t)
		if err != nil {
			return nil, nil, nil, err
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		targets = append(targets, t)
		srcs = append(srcs, r)
	}
	return lay, targets, srcs, nil
}

func (x *Executor) execUpdate(ctx context.Context, n *ir.Update, e *env) (*Result, error) {
	alias := ir.AliasOr(n.Alias, n.Rel)
	lay, targets, srcs, err := targetRows(ctx, n.Rel, alias, n.Input, e)
	if err != nil {
		return nil, err
	}
	set, err := compileAssigns(n.Rel, n.Set, lay)
	if err != nil {
		return nil, err
	}
	m := newMutation(n.Rel)
	for i, old := range targets {
		upd, err := assign(set, old, srcs[i], e)
		if err != nil {
			return nil, err
		}
		if err := m.add(storage.Mutation{Kind: storage.Update, Rel: n.Rel, Old: old, New: upd}); err != nil {
			return nil, err
		}
	}
	return m.apply(ctx, e, alias, n.Returning)
}

func (x *Executor) execDelete(ctx context.Context, n *ir.Delete, e *env) (*Result, error) {
	alias := ir.AliasOr(n.Alias, n.Rel)
	_, targets, _, err := targetRows(ctx, n.Rel, alias, n.Input, e)
	if err != nil {
		return nil, err
	}
	m := newMutation(n.Rel)
	for _, old := range targets {
		if err := m.add(storage.Mutation{Kind: storage.Delete, Rel: n.Rel, Old: old}); err != nil {
			return nil, err
		}
	}
	return m.apply(ctx, e, alias, n.Returning)
}
