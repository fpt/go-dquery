package opt

import "github.com/fpt/go-dquery/ir"

// withChildren returns a shallow copy of n whose inputs (and Map field
// plans) are replaced by f(child). n itself is not modified.
func withChildren(n ir.Node, f func(ir.Node) (ir.Node, error)) (ir.Node, error) {
	var err error
	one := func(in ir.Node) ir.Node {
		if err != nil || in == nil {
			return in
		}
		var out ir.Node
		out, err = f(in)
		return out
	}
	switch n := n.(type) {
	case *ir.Filter:
		c := *n
		c.Input = one(n.Input)
		return &c, err
	case *ir.Project:
		c := *n
		c.Input = one(n.Input)
		return &c, err
	case *ir.Lookup:
		c := *n
		c.Input = one(n.Input)
		return &c, err
	case *ir.Map:
		c := *n
		c.Input = one(n.Input)
		c.Fields = make([]ir.MapField, len(n.Fields))
		for i, fd := range n.Fields {
			fd.Plan = one(fd.Plan)
			c.Fields[i] = fd
		}
		return &c, err
	case *ir.Limit:
		c := *n
		c.Input = one(n.Input)
		return &c, err
	case *ir.Sort:
		c := *n
		c.Input = one(n.Input)
		return &c, err
	case *ir.Return:
		c := *n
		c.Input = one(n.Input)
		return &c, err
	case *ir.Insert:
		c := *n
		c.Input = one(n.Input)
		return &c, err
	case *ir.Update:
		c := *n
		c.Input = one(n.Input)
		return &c, err
	case *ir.Delete:
		c := *n
		c.Input = one(n.Input)
		return &c, err
	}
	return n, nil // leaves
}

// transform rewrites a plan bottom-up.
func transform(n ir.Node, f func(ir.Node) (ir.Node, error)) (ir.Node, error) {
	c, err := withChildren(n, func(in ir.Node) (ir.Node, error) { return transform(in, f) })
	if err != nil {
		return nil, err
	}
	return f(c)
}

// walkExpr calls f for e and every sub-expression.
func walkExpr(e ir.Expr, f func(ir.Expr)) {
	if e == nil {
		return
	}
	f(e)
	switch e := e.(type) {
	case ir.Cmp:
		walkExpr(e.L, f)
		walkExpr(e.R, f)
	case ir.And:
		for _, t := range e.Terms {
			walkExpr(t, f)
		}
	case ir.Or:
		for _, t := range e.Terms {
			walkExpr(t, f)
		}
	case ir.Not:
		walkExpr(e.X, f)
	case ir.In:
		walkExpr(e.X, f)
		for _, t := range e.List {
			walkExpr(t, f)
		}
	case ir.IsNull:
		walkExpr(e.X, f)
	case ir.Arith:
		walkExpr(e.L, f)
		walkExpr(e.R, f)
	}
}

// nodeExprs returns the expressions held directly by n (not by its inputs).
func nodeExprs(n ir.Node) []ir.Expr {
	var es []ir.Expr
	named := func(ns []ir.NamedExpr) {
		for _, ne := range ns {
			es = append(es, ne.Expr)
		}
	}
	switch n := n.(type) {
	case *ir.Get:
		es = append(es, n.Key...)
	case *ir.GetMany:
		for _, k := range n.Keys {
			es = append(es, k...)
		}
	case *ir.Scan:
		es = append(es, n.Eq...)
		for _, b := range []*ir.Bound{n.Lo, n.Hi} {
			if b != nil {
				es = append(es, b.Expr)
			}
		}
	case *ir.Filter:
		es = append(es, n.Pred)
	case *ir.Project:
		named(n.Exprs)
	case *ir.Lookup:
		es = append(es, n.Key...)
	case *ir.Sort:
		for _, k := range n.Keys {
			es = append(es, k.Col)
		}
	case *ir.Insert:
		for _, r := range n.Rows {
			es = append(es, r...)
		}
		if n.OnConflict != nil {
			for _, a := range n.OnConflict.Set {
				es = append(es, a.Expr)
			}
		}
		named(n.Returning)
	case *ir.Update:
		for _, a := range n.Set {
			es = append(es, a.Expr)
		}
		named(n.Returning)
	case *ir.Delete:
		named(n.Returning)
	}
	return es
}

// colRefs returns the column references in es.
func colRefs(es ...ir.Expr) []ir.Col {
	var out []ir.Col
	for _, e := range es {
		walkExpr(e, func(x ir.Expr) {
			if c, ok := x.(ir.Col); ok {
				out = append(out, c)
			}
		})
	}
	return out
}

// outerRefs returns the Outer references of a Map field plan that bind to
// the enclosing Map's input. Nested Map fields have their own scope and are
// skipped.
func outerRefs(plan ir.Node) []ir.Outer {
	var out []ir.Outer
	var visit func(ir.Node)
	visit = func(n ir.Node) {
		for _, e := range nodeExprs(n) {
			walkExpr(e, func(x ir.Expr) {
				if o, ok := x.(ir.Outer); ok {
					out = append(out, o)
				}
			})
		}
		if m, ok := n.(*ir.Map); ok {
			visit(m.Input)
			return
		}
		for _, in := range n.Inputs() {
			visit(in)
		}
	}
	visit(plan)
	return out
}

// conjuncts flattens nested ANDs.
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

// conjoin is the inverse of conjuncts; it returns nil for no terms.
func conjoin(es []ir.Expr) ir.Expr {
	switch len(es) {
	case 0:
		return nil
	case 1:
		return es[0]
	}
	return ir.And{Terms: es}
}

// filtered wraps n in a Filter over the given conjuncts, if any.
func filtered(n ir.Node, conj []ir.Expr) ir.Node {
	if p := conjoin(conj); p != nil {
		return &ir.Filter{Input: n, Pred: p}
	}
	return n
}

// bindable reports whether e can be evaluated without the current row, i.e.
// it can serve as an access-path key.
func bindable(e ir.Expr) bool { return len(colRefs(e)) == 0 }
