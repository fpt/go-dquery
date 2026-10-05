// Package opt is the rule-based IR optimizer.
//
// Rules, in order:
//
//  1. Predicate pushdown: filters move below Lookup and Map when they only
//     reference the input side.
//  2. Access-path selection: Filter over a full Scan becomes Get, GetMany or a
//     bounded Scan on the cheapest access path; unused conjuncts remain in a
//     Filter. PK equality yielding Get is the cheapest case.
//  3. Order satisfaction: Sort is removed by choosing a path (and direction)
//     whose order satisfies it; otherwise optimization fails with ErrOrder.
//  4. Limit pushdown into Scan and into per-row Lookup limits.
//  5. Projection pushdown: each access gets the set of columns it must
//     provide.
//  6. Map batching: Map fields with a single Get/Scan leaf are marked batched.
//
// Finally, unbounded full scans are rejected unless Options.AllowFullScan.
package opt

import (
	"errors"
	"fmt"
	"slices"

	"github.com/fpt/go-dquery/ir"
)

type Options struct {
	// AllowFullScan permits scans that read a whole access path without a
	// limit.
	AllowFullScan bool
}

var (
	// ErrFullScan reports a plan that would scan a whole relation.
	ErrFullScan = errors.New("opt: query requires an unbounded full scan")
	// ErrOrder reports an ORDER BY that no access path can provide.
	ErrOrder = errors.New("opt: no access path provides the requested order")
)

// Optimize rewrites a plan. The input plan is not modified.
func Optimize(n ir.Node, o Options) (ir.Node, error) {
	if err := ir.Validate(n); err != nil {
		return nil, err
	}
	r := &rewriter{opts: o}
	n, err := r.rewrite(n)
	if err != nil {
		return nil, err
	}
	n = prune(n, need{all: true})
	if n, err = transform(n, batchMap); err != nil {
		return nil, err
	}
	if err := check(n, o); err != nil {
		return nil, err
	}
	return n, ir.Validate(n)
}

type rewriter struct{ opts Options }

func (r *rewriter) rewrite(n ir.Node) (ir.Node, error) {
	switch n := n.(type) {
	case *ir.Filter:
		return r.filter(conjuncts(n.Pred), n.Input, nil)
	case *ir.Sort:
		return r.ordered(n.Input, n.Keys)
	case *ir.Limit:
		in, err := r.rewrite(n.Input)
		if err != nil {
			return nil, err
		}
		return limit(in, n.N), nil
	}
	return withChildren(n, r.rewrite)
}

// rewriteOrdered rewrites n, additionally requiring order when it is set.
func (r *rewriter) rewriteOrdered(n ir.Node, order []ir.SortKey) (ir.Node, error) {
	if order == nil {
		return r.rewrite(n)
	}
	return r.ordered(n, order)
}

// filter rewrites Filter(conj, input), pushing conjuncts down and selecting
// access paths. order, if set, is required of the result.
func (r *rewriter) filter(conj []ir.Expr, input ir.Node, order []ir.SortKey) (ir.Node, error) {
	switch in := input.(type) {
	case *ir.Filter:
		return r.filter(append(slices.Clone(conj), conjuncts(in.Pred)...), in.Input, order)

	case *ir.Scan:
		if isPlainFull(in) {
			return r.access(in, conj, order)
		}

	case *ir.Lookup:
		push, keep := split(conj, func(e ir.Expr) bool {
			for _, c := range colRefs(e) {
				if owns(c, in.Rel, ir.AliasOr(in.Alias, in.Rel)) {
					return false
				}
			}
			return true
		})
		if len(push) > 0 {
			lk := *in
			lk.Input = &ir.Filter{Input: in.Input, Pred: conjoin(push)}
			out, err := r.rewriteOrdered(&lk, order)
			if err != nil {
				return nil, err
			}
			return filtered(out, keep), nil
		}

	case *ir.Map:
		names := map[string]bool{}
		for _, f := range in.Fields {
			names[f.Name] = true
		}
		push, keep := split(conj, func(e ir.Expr) bool {
			for _, c := range colRefs(e) {
				if c.Qual == "" && names[c.Name] {
					return false
				}
			}
			return true
		})
		if len(push) > 0 {
			m := *in
			m.Input = &ir.Filter{Input: in.Input, Pred: conjoin(push)}
			out, err := r.rewriteOrdered(&m, order)
			if err != nil {
				return nil, err
			}
			return filtered(out, keep), nil
		}
	}
	out, err := r.rewriteOrdered(input, order)
	if err != nil {
		return nil, err
	}
	return filtered(out, conj), nil
}

// ordered rewrites n so that its output satisfies keys, removing the Sort.
func (r *rewriter) ordered(n ir.Node, keys []ir.SortKey) (ir.Node, error) {
	switch n := n.(type) {
	case *ir.Sort:
		return r.ordered(n.Input, keys) // the outer order wins
	case *ir.Filter:
		return r.filter(conjuncts(n.Pred), n.Input, keys)
	case *ir.Get:
		return n, nil
	case *ir.Scan:
		if isPlainFull(n) {
			return r.access(n, nil, keys)
		}
		rev, ok := satisfies(n.Rel, ir.AliasOr(n.Alias, n.Rel), n.Path, len(n.Eq), keys)
		if !ok {
			return nil, fmt.Errorf("%w: %s via %s by %s", ErrOrder, n.Rel.Name, n.Path.Name, sortString(keys))
		}
		c := *n
		c.Reverse = rev
		return &c, nil
	case *ir.Lookup:
		alias := ir.AliasOr(n.Alias, n.Rel)
		onTarget := 0
		for _, k := range keys {
			if owns(k.Col, n.Rel, alias) {
				onTarget++
			}
		}
		c := *n
		var err error
		switch onTarget {
		case 0: // Lookup preserves input order
			c.Input, err = r.ordered(n.Input, keys)
			return &c, err
		case len(keys):
			if c.Input, err = r.rewrite(n.Input); err != nil {
				return nil, err
			}
			if c.Input.Shape() != ir.ShapeOne {
				return nil, fmt.Errorf("%w: ordering by %s needs a single outer row", ErrOrder, alias)
			}
			if !n.Many {
				return &c, nil
			}
			rev, ok := satisfies(n.Rel, alias, n.Path, len(n.Key), keys)
			if !ok {
				return nil, fmt.Errorf("%w: %s via %s by %s", ErrOrder, n.Rel.Name, n.Path.Name, sortString(keys))
			}
			c.Reverse = rev
			return &c, nil
		}
	case *ir.Map: // Map preserves input order
		c := *n
		var err error
		if c.Input, err = r.ordered(n.Input, keys); err != nil {
			return nil, err
		}
		c.Fields = slices.Clone(n.Fields)
		for i := range c.Fields {
			if c.Fields[i].Plan, err = r.rewrite(c.Fields[i].Plan); err != nil {
				return nil, err
			}
		}
		return &c, nil
	}
	return nil, fmt.Errorf("%w: cannot order %T by %s", ErrOrder, n, sortString(keys))
}

// limit pushes a limit into n where that is equivalent, and keeps an
// explicit Limit otherwise.
func limit(n ir.Node, count int) ir.Node {
	if out, ok := pushLimit(n, count); ok {
		return out
	}
	return &ir.Limit{Input: n, N: count}
}

// pushLimit returns n with the limit absorbed, and whether it was. When it
// returns false, the result is equivalent to n without any limit.
func pushLimit(n ir.Node, count int) (ir.Node, bool) {
	if count <= 0 {
		return n, false
	}
	switch n := n.(type) {
	case *ir.Scan:
		c := *n
		if c.Limit == 0 || count < c.Limit {
			c.Limit = count
		}
		return &c, true
	case *ir.Get:
		return n, true
	case *ir.Project:
		in, ok := pushLimit(n.Input, count)
		c := *n
		c.Input = in
		return &c, ok
	case *ir.Map:
		in, ok := pushLimit(n.Input, count)
		c := *n
		c.Input = in
		return &c, ok
	case *ir.Lookup:
		c := *n
		switch {
		case n.Optional && !n.Many: // exactly one output row per input row
			var ok bool
			c.Input, ok = pushLimit(n.Input, count)
			return &c, ok
		case n.Many && n.Input.Shape() == ir.ShapeOne:
			if c.Limit == 0 || count < c.Limit {
				c.Limit = count
			}
			return &c, true
		}
	case *ir.Limit:
		if count >= n.N {
			return n, true
		}
	}
	return n, false
}

func isPlainFull(s *ir.Scan) bool {
	return len(s.Eq) == 0 && s.Lo == nil && s.Hi == nil && s.Limit == 0 && !s.Reverse
}

func split(es []ir.Expr, pred func(ir.Expr) bool) (yes, no []ir.Expr) {
	for _, e := range es {
		if pred(e) {
			yes = append(yes, e)
		} else {
			no = append(no, e)
		}
	}
	return yes, no
}

// batchMap marks Map fields whose plan can be fetched for a whole batch.
func batchMap(n ir.Node) (ir.Node, error) {
	m, ok := n.(*ir.Map)
	if !ok {
		return n, nil
	}
	c := *m
	c.Fields = slices.Clone(m.Fields)
	for i := range c.Fields {
		c.Fields[i].Batched = ir.BatchLeaf(c.Fields[i].Plan) != nil
	}
	return &c, nil
}

// check rejects plans that remain unsatisfiable or too expensive.
func check(n ir.Node, o Options) error {
	var err error
	var visit func(ir.Node)
	visit = func(n ir.Node) {
		if err != nil {
			return
		}
		switch n := n.(type) {
		case *ir.Sort:
			err = fmt.Errorf("%w: %s", ErrOrder, sortString(n.Keys))
		case *ir.Scan:
			if !o.AllowFullScan && len(n.Eq) == 0 && n.Lo == nil && n.Hi == nil && n.Limit == 0 {
				err = fmt.Errorf("%w: %s", ErrFullScan, n.Rel.Name)
			}
		}
		for _, in := range n.Inputs() {
			visit(in)
		}
	}
	visit(n)
	return err
}
