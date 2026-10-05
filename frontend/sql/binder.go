package sql

import (
	"fmt"
	"slices"

	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/opt"
	"github.com/fpt/go-dquery/schema"
)

// Compiled is a statement ready to execute.
type Compiled struct {
	Stmt    Statement
	Logical ir.Node // bound plan before optimization
	Plan    ir.Node // optimized plan
	Explain bool    // the statement was EXPLAIN <stmt>
}

// Compile parses, binds and optimizes a single statement.
func Compile(cat *schema.Catalog, src string, o opt.Options) (*Compiled, error) {
	stmt, err := Parse(src)
	if err != nil {
		return nil, err
	}
	return CompileStmt(cat, stmt, o)
}

// CompileStmt binds and optimizes a parsed statement.
func CompileStmt(cat *schema.Catalog, stmt Statement, o opt.Options) (*Compiled, error) {
	c := &Compiled{Stmt: stmt}
	if e, ok := stmt.(*Explain); ok {
		c.Explain = true
		stmt = e.Stmt
	}
	var err error
	if c.Logical, err = Bind(cat, stmt); err != nil {
		return nil, err
	}
	if c.Plan, err = opt.Optimize(c.Logical, o); err != nil {
		return nil, err
	}
	return c, nil
}

// Bind resolves names against the catalog and builds an unoptimized plan.
// Joins are bound to Lookups on an access path of the joined table.
func Bind(cat *schema.Catalog, stmt Statement) (ir.Node, error) {
	b := &binder{cat: cat}
	switch s := stmt.(type) {
	case *Select:
		return b.selectStmt(s)
	case *Insert:
		return b.insertStmt(s)
	case *Update:
		return b.updateStmt(s)
	case *Delete:
		return b.deleteStmt(s)
	case *Explain:
		return nil, fmt.Errorf("sql: cannot bind EXPLAIN; use CompileStmt")
	}
	return nil, fmt.Errorf("sql: unknown statement %T", stmt)
}

type binder struct{ cat *schema.Catalog }

type scopeTable struct {
	ref string
	rel *schema.Relation
}

// scope is the set of tables visible to expressions, in FROM order.
type scope []scopeTable

func (s scope) find(ref string) *scopeTable {
	for i := range s {
		if s[i].ref == ref {
			return &s[i]
		}
	}
	return nil
}

// resolve qualifies a column reference.
func (s scope) resolve(c ir.Col) (ir.Col, error) {
	if c.Qual != "" {
		t := s.find(c.Qual)
		if t == nil {
			return c, fmt.Errorf("sql: unknown table %q", c.Qual)
		}
		if t.rel.Column(c.Name) == nil {
			return c, fmt.Errorf("sql: %s has no column %q", t.rel.Name, c.Name)
		}
		return c, nil
	}
	var found []string
	for _, t := range s {
		if t.rel.Column(c.Name) != nil {
			found = append(found, t.ref)
		}
	}
	switch len(found) {
	case 0:
		return c, fmt.Errorf("sql: unknown column %q", c.Name)
	case 1:
		return ir.C(found[0], c.Name), nil
	}
	return c, fmt.Errorf("sql: column %q is ambiguous (in %v)", c.Name, found)
}

// qualify resolves every column reference in e.
func (s scope) qualify(e ir.Expr) (ir.Expr, error) {
	if e == nil {
		return nil, nil
	}
	var err error
	q := func(x ir.Expr) ir.Expr {
		if err != nil {
			return x
		}
		var out ir.Expr
		out, err = s.qualify(x)
		return out
	}
	qs := func(xs []ir.Expr) []ir.Expr {
		out := make([]ir.Expr, len(xs))
		for i, x := range xs {
			out[i] = q(x)
		}
		return out
	}
	switch e := e.(type) {
	case ir.Col:
		return s.resolve(e)
	case ir.Cmp:
		return ir.Cmp{Op: e.Op, L: q(e.L), R: q(e.R)}, err
	case ir.And:
		return ir.And{Terms: qs(e.Terms)}, err
	case ir.Or:
		return ir.Or{Terms: qs(e.Terms)}, err
	case ir.Not:
		return ir.Not{X: q(e.X)}, err
	case ir.In:
		return ir.In{X: q(e.X), List: qs(e.List)}, err
	case ir.IsNull:
		return ir.IsNull{X: q(e.X), Negate: e.Negate}, err
	case ir.Arith:
		return ir.Arith{Op: e.Op, L: q(e.L), R: q(e.R)}, err
	}
	return e, nil
}

func (b *binder) table(t TableRef, sc scope) (scopeTable, error) {
	rel := b.cat.Relation(t.Name)
	if rel == nil {
		return scopeTable{}, fmt.Errorf("sql: unknown table %q", t.Name)
	}
	if sc.find(t.Ref()) != nil {
		return scopeTable{}, fmt.Errorf("sql: table name %q specified more than once", t.Ref())
	}
	return scopeTable{ref: t.Ref(), rel: rel}, nil
}

func (b *binder) selectStmt(s *Select) (ir.Node, error) {
	from, err := b.table(s.From, nil)
	if err != nil {
		return nil, err
	}
	sc := scope{from}
	var n ir.Node = &ir.Scan{Rel: from.rel, Alias: from.ref, Path: from.rel.Primary}
	for _, j := range s.Joins {
		if n, sc, err = b.join(n, sc, j); err != nil {
			return nil, err
		}
	}
	if s.Where != nil {
		w, err := sc.qualify(s.Where)
		if err != nil {
			return nil, err
		}
		n = &ir.Filter{Input: n, Pred: w}
	}
	items, err := expandItems(s.Items, sc)
	if err != nil {
		return nil, err
	}
	if len(s.OrderBy) > 0 {
		keys := make([]ir.SortKey, len(s.OrderBy))
		for i, o := range s.OrderBy {
			col, err := orderCol(o.Expr, sc, items)
			if err != nil {
				return nil, err
			}
			keys[i] = ir.SortKey{Col: col, Desc: o.Desc}
		}
		n = &ir.Sort{Input: n, Keys: keys}
	}
	if s.Limit >= 0 {
		n = &ir.Limit{Input: n, N: s.Limit}
	}
	return &ir.Project{Input: n, Exprs: items}, nil
}

// orderCol resolves an ORDER BY item to a table column, either directly or
// through a select-list alias.
func orderCol(e ir.Expr, sc scope, items []ir.NamedExpr) (ir.Col, error) {
	c, ok := e.(ir.Col)
	if !ok {
		return ir.Col{}, fmt.Errorf("%w: ORDER BY expressions (only columns)", ErrNotSupported)
	}
	if c.Qual == "" && slices.ContainsFunc(sc, func(t scopeTable) bool { return t.rel.Column(c.Name) != nil }) {
		return sc.resolve(c)
	}
	if c.Qual == "" {
		for _, it := range items {
			if ic, ok := it.Expr.(ir.Col); ok && it.Name == c.Name {
				return ic, nil
			}
		}
	}
	return sc.resolve(c)
}

// join binds "JOIN t ON ..." to a Lookup: equality conjuncts between t's
// columns and expressions over earlier tables must bind a key prefix of one
// of t's access paths. Remaining conjuncts become a Filter (inner joins only).
func (b *binder) join(input ir.Node, sc scope, j Join) (ir.Node, scope, error) {
	t, err := b.table(j.Table, sc)
	if err != nil {
		return nil, nil, err
	}
	inner := append(slices.Clone(sc), t)
	on, err := inner.qualify(j.On)
	if err != nil {
		return nil, nil, err
	}
	conj := conjuncts(on)

	// eqs maps a column of t to the conjunct binding it.
	type binding struct {
		idx  int
		expr ir.Expr
	}
	eqs := map[schema.ColID]binding{}
	refsT := func(e ir.Expr) bool {
		found := false
		walkCols(e, func(c ir.Col) { found = found || c.Qual == t.ref })
		return found
	}
	for i, e := range conj {
		cmp, ok := e.(ir.Cmp)
		if !ok || cmp.Op != ir.OpEq {
			continue
		}
		for _, side := range [][2]ir.Expr{{cmp.L, cmp.R}, {cmp.R, cmp.L}} {
			c, ok := side[0].(ir.Col)
			if !ok || c.Qual != t.ref || refsT(side[1]) {
				continue
			}
			id := t.rel.Column(c.Name).ID
			if _, dup := eqs[id]; !dup {
				eqs[id] = binding{idx: i, expr: side[1]}
			}
			break
		}
	}

	var best *schema.AccessPath
	var bestKey []ir.Expr
	var bestUsed []int
	bestScore := -1
	for _, p := range t.rel.AllPaths() {
		var key []ir.Expr
		var used []int
		for _, kc := range p.KeyCols() {
			bd, ok := eqs[kc]
			if !ok {
				break
			}
			key = append(key, bd.expr)
			used = append(used, bd.idx)
		}
		if len(key) == 0 || len(key) < len(p.Partition) {
			continue
		}
		score := len(key)
		if p.Unique && len(key) == len(p.KeyCols()) {
			score += 1000 // a unique match yields at most one row
		}
		if score > bestScore {
			best, bestKey, bestUsed, bestScore = p, key, used, score
		}
	}
	if best == nil {
		return nil, nil, fmt.Errorf("%w: JOIN %s: ON must bind the key of an access path of %s with equalities",
			ErrNotSupported, t.ref, t.rel.Name)
	}
	var residual []ir.Expr
	for i, e := range conj {
		if !slices.Contains(bestUsed, i) {
			residual = append(residual, e)
		}
	}
	unique := best.Unique && len(bestKey) == len(best.KeyCols())
	var n ir.Node = &ir.Lookup{
		Input: input, Rel: t.rel, Alias: t.ref, Path: best, Key: bestKey,
		Many: !unique, Optional: j.Left,
	}
	if len(residual) > 0 {
		if j.Left {
			return nil, nil, fmt.Errorf("%w: LEFT JOIN %s: ON conditions beyond the access path key", ErrNotSupported, t.ref)
		}
		n = &ir.Filter{Input: n, Pred: conjoin(residual)}
	}
	return n, inner, nil
}

// expandItems resolves a select or RETURNING list.
func expandItems(items []SelectItem, sc scope) ([]ir.NamedExpr, error) {
	var out []ir.NamedExpr
	for _, it := range items {
		if it.Star {
			matched := false
			for _, t := range sc {
				if it.Qual != "" && it.Qual != t.ref {
					continue
				}
				matched = true
				for _, c := range t.rel.Columns {
					out = append(out, ir.NamedExpr{Name: c.Name, Expr: ir.C(t.ref, c.Name)})
				}
			}
			if !matched {
				return nil, fmt.Errorf("sql: unknown table %q", it.Qual)
			}
			continue
		}
		e, err := sc.qualify(it.Expr)
		if err != nil {
			return nil, err
		}
		name := it.Alias
		if name == "" {
			if c, ok := e.(ir.Col); ok {
				name = c.Name
			} else {
				name = it.Expr.String()
			}
		}
		out = append(out, ir.NamedExpr{Name: name, Expr: e})
	}
	return out, nil
}

func (b *binder) insertStmt(s *Insert) (ir.Node, error) {
	t, err := b.table(s.Table, nil)
	if err != nil {
		return nil, err
	}
	n := &ir.Insert{Rel: t.rel, Alias: t.ref, Cols: s.Cols}
	for _, row := range s.Rows {
		for _, e := range row {
			if len(colRefsOf(e)) > 0 {
				return nil, fmt.Errorf("sql: column references are not allowed in VALUES")
			}
		}
		n.Rows = append(n.Rows, row)
	}
	if c := s.Conflict; c != nil {
		if c.Target != nil {
			var pk []string
			for _, id := range t.rel.Primary.KeyCols() {
				pk = append(pk, t.rel.Columns[id].Name)
			}
			if !sameSet(c.Target, pk) {
				return nil, fmt.Errorf("%w: ON CONFLICT target must be the primary key %v", ErrNotSupported, pk)
			}
		}
		oc := &ir.OnConflict{DoNothing: c.DoNothing}
		withExcluded := scope{t, {ref: "excluded", rel: t.rel}}
		for _, a := range c.Set {
			e, err := withExcluded.qualify(a.Expr)
			if err != nil {
				return nil, err
			}
			oc.Set = append(oc.Set, ir.Assign{Col: a.Col, Expr: e})
		}
		n.OnConflict = oc
	}
	if n.Returning, err = returning(s.Returning, scope{t}); err != nil {
		return nil, err
	}
	return n, nil
}

func (b *binder) target(t TableRef, where ir.Expr) (scopeTable, ir.Node, error) {
	st, err := b.table(t, nil)
	if err != nil {
		return st, nil, err
	}
	var n ir.Node = &ir.Scan{Rel: st.rel, Alias: st.ref, Path: st.rel.Primary}
	if where != nil {
		w, err := scope{st}.qualify(where)
		if err != nil {
			return st, nil, err
		}
		n = &ir.Filter{Input: n, Pred: w}
	}
	return st, n, nil
}

func (b *binder) updateStmt(s *Update) (ir.Node, error) {
	t, input, err := b.target(s.Table, s.Where)
	if err != nil {
		return nil, err
	}
	n := &ir.Update{Rel: t.rel, Alias: t.ref, Input: input}
	for _, a := range s.Set {
		e, err := scope{t}.qualify(a.Expr)
		if err != nil {
			return nil, err
		}
		n.Set = append(n.Set, ir.Assign{Col: a.Col, Expr: e})
	}
	if n.Returning, err = returning(s.Returning, scope{t}); err != nil {
		return nil, err
	}
	return n, nil
}

func (b *binder) deleteStmt(s *Delete) (ir.Node, error) {
	t, input, err := b.target(s.Table, s.Where)
	if err != nil {
		return nil, err
	}
	n := &ir.Delete{Rel: t.rel, Alias: t.ref, Input: input}
	if n.Returning, err = returning(s.Returning, scope{t}); err != nil {
		return nil, err
	}
	return n, nil
}

func returning(items []SelectItem, sc scope) ([]ir.NamedExpr, error) {
	if items == nil {
		return nil, nil
	}
	return expandItems(items, sc)
}

func conjuncts(e ir.Expr) []ir.Expr {
	if a, ok := e.(ir.And); ok {
		var out []ir.Expr
		for _, t := range a.Terms {
			out = append(out, conjuncts(t)...)
		}
		return out
	}
	return []ir.Expr{e}
}

func conjoin(es []ir.Expr) ir.Expr {
	if len(es) == 1 {
		return es[0]
	}
	return ir.And{Terms: es}
}

func walkCols(e ir.Expr, f func(ir.Col)) {
	switch e := e.(type) {
	case ir.Col:
		f(e)
	case ir.Cmp:
		walkCols(e.L, f)
		walkCols(e.R, f)
	case ir.And:
		for _, t := range e.Terms {
			walkCols(t, f)
		}
	case ir.Or:
		for _, t := range e.Terms {
			walkCols(t, f)
		}
	case ir.Not:
		walkCols(e.X, f)
	case ir.In:
		walkCols(e.X, f)
		for _, t := range e.List {
			walkCols(t, f)
		}
	case ir.IsNull:
		walkCols(e.X, f)
	case ir.Arith:
		walkCols(e.L, f)
		walkCols(e.R, f)
	}
}

func colRefsOf(e ir.Expr) []ir.Col {
	var out []ir.Col
	walkCols(e, func(c ir.Col) { out = append(out, c) })
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}
