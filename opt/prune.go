package opt

import (
	"maps"

	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/schema"
)

// need is the set of columns a consumer requires from its input.
type need struct {
	all  bool
	cols map[ir.Col]bool
}

func (nd need) with(cols ...ir.Col) need {
	if nd.all {
		return nd
	}
	out := need{cols: maps.Clone(nd.cols)}
	if out.cols == nil {
		out.cols = map[ir.Col]bool{}
	}
	for _, c := range cols {
		out.cols[c] = true
	}
	return out
}

// colsFor returns the projection for one access, or nil for all columns.
func (nd need) colsFor(rel *schema.Relation, alias string) []schema.ColID {
	if nd.all {
		return nil
	}
	out := []schema.ColID{}
	for _, c := range rel.Columns {
		if nd.cols[ir.Col{Qual: alias, Name: c.Name}] || nd.cols[ir.Col{Name: c.Name}] {
			out = append(out, c.ID)
		}
	}
	if len(out) == len(rel.Columns) {
		return nil
	}
	return out
}

// prune sets Cols on every access to the columns its consumers reference.
func prune(n ir.Node, nd need) ir.Node {
	switch n := n.(type) {
	case *ir.Return:
		c := *n
		c.Input = prune(n.Input, nd)
		return &c
	case *ir.Project:
		c := *n
		c.Input = prune(n.Input, need{}.with(colRefs(nodeExprs(n)...)...))
		return &c
	case *ir.Filter:
		c := *n
		c.Input = prune(n.Input, nd.with(colRefs(n.Pred)...))
		return &c
	case *ir.Limit:
		c := *n
		c.Input = prune(n.Input, nd)
		return &c
	case *ir.Sort:
		c := *n
		c.Input = prune(n.Input, nd.with(colRefs(nodeExprs(n)...)...))
		return &c
	case *ir.Lookup:
		c := *n
		c.Input = prune(n.Input, nd.with(colRefs(n.Key...)...))
		c.Cols = nd.colsFor(n.Rel, ir.AliasOr(n.Alias, n.Rel))
		return &c
	case *ir.Map:
		c := *n
		inNeed := nd
		c.Fields = make([]ir.MapField, len(n.Fields))
		for i, f := range n.Fields {
			for _, o := range outerRefs(f.Plan) {
				inNeed = inNeed.with(ir.Col{Qual: o.Qual, Name: o.Name})
			}
			f.Plan = prune(f.Plan, need{all: true})
			c.Fields[i] = f
		}
		c.Input = prune(n.Input, inNeed)
		return &c
	case *ir.Get:
		c := *n
		c.Cols = nd.colsFor(n.Rel, ir.AliasOr(n.Alias, n.Rel))
		return &c
	case *ir.GetMany:
		c := *n
		c.Cols = nd.colsFor(n.Rel, ir.AliasOr(n.Alias, n.Rel))
		return &c
	case *ir.Scan:
		c := *n
		c.Cols = nd.colsFor(n.Rel, ir.AliasOr(n.Alias, n.Rel))
		return &c
	case *ir.Insert, *ir.Update, *ir.Delete:
		// Mutations need full row images.
		out, _ := withChildren(n, func(in ir.Node) (ir.Node, error) { return prune(in, need{all: true}), nil })
		return out
	}
	return n
}
