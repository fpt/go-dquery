package opt

import (
	"fmt"
	"slices"
	"strings"

	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/schema"
)

// Access cost ranking (rule-based; no statistics yet).
const (
	costPrimaryKey = 1
	costUniqueKey  = 2
	costMultiGet   = 3
	costPrefix     = 10
	costRange      = 50
	costFullScan   = 10000
)

// atom is a conjunct usable for access-path selection, normalized to
// "col op rhs" (or "col IN list").
type atom struct {
	idx  int // position in the conjunct list
	col  schema.ColID
	op   ir.CmpOp // zero for IN
	rhs  ir.Expr
	list []ir.Expr
}

// owns reports whether c refers to a column of rel under alias.
func owns(c ir.Col, rel *schema.Relation, alias string) bool {
	return (c.Qual == "" || c.Qual == alias) && rel.Column(c.Name) != nil
}

func atoms(conj []ir.Expr, rel *schema.Relation, alias string) []atom {
	var out []atom
	colOf := func(e ir.Expr) (*schema.Column, bool) {
		c, ok := e.(ir.Col)
		if !ok || !owns(c, rel, alias) {
			return nil, false
		}
		return rel.Column(c.Name), true
	}
	for i, e := range conj {
		switch e := e.(type) {
		case ir.Cmp:
			if e.Op == ir.OpNe {
				continue
			}
			if col, ok := colOf(e.L); ok && bindable(e.R) && litFits(col, e.R) {
				out = append(out, atom{idx: i, col: col.ID, op: e.Op, rhs: e.R})
			} else if col, ok := colOf(e.R); ok && bindable(e.L) && litFits(col, e.L) {
				out = append(out, atom{idx: i, col: col.ID, op: e.Op.Flip(), rhs: e.L})
			}
		case ir.In:
			col, ok := colOf(e.X)
			if !ok || len(e.List) == 0 {
				continue
			}
			fits := true
			for _, x := range e.List {
				fits = fits && bindable(x) && litFits(col, x)
			}
			if fits {
				out = append(out, atom{idx: i, col: col.ID, list: e.List})
			}
		}
	}
	return out
}

// litFits rejects literals that cannot be coerced to the key column type,
// where a filter would compare across types (e.g. int column = 1.5) but a
// key lookup would fail.
func litFits(col *schema.Column, e ir.Expr) bool {
	l, ok := e.(ir.Lit)
	if !ok || l.V.IsNull() {
		return true
	}
	_, err := col.Coerce(l.V)
	return err == nil
}

type candidate struct {
	path     *schema.AccessPath
	eq       []ir.Expr
	in       []ir.Expr // GetMany keys (single-column unique path)
	lo, hi   *ir.Bound
	reverse  bool
	used     []int
	cost     int
	pathRank int
}

func (c *candidate) better(o *candidate) bool {
	if o == nil {
		return true
	}
	if c.cost != o.cost {
		return c.cost < o.cost
	}
	if len(c.used) != len(o.used) {
		return len(c.used) > len(o.used)
	}
	return c.pathRank < o.pathRank
}

// access chooses the best access path for a full scan of rel filtered by
// conj and, when order is set, producing rows in that order.
func (r *rewriter) access(scan *ir.Scan, conj []ir.Expr, order []ir.SortKey) (ir.Node, error) {
	rel := scan.Rel
	alias := ir.AliasOr(scan.Alias, rel)
	as := atoms(conj, rel, alias)
	var best *candidate
	for rank, p := range rel.AllPaths() {
		for _, c := range []*candidate{keyed(p, as, rel, alias, order), multiGet(p, as, order), fullScan(p, rel, alias, order)} {
			if c != nil {
				c.pathRank = rank
				if c.better(best) {
					best = c
				}
			}
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: %s by %s", ErrOrder, rel.Name, sortString(order))
	}
	var residual []ir.Expr
	for i, e := range conj {
		if !slices.Contains(best.used, i) {
			residual = append(residual, e)
		}
	}
	var n ir.Node
	switch {
	case best.in != nil:
		keys := make([][]ir.Expr, len(best.in))
		for i, e := range best.in {
			keys[i] = []ir.Expr{e}
		}
		n = &ir.GetMany{Rel: rel, Alias: scan.Alias, Path: best.path, Keys: keys, Cols: scan.Cols}
	case best.path.Unique && len(best.eq) == len(best.path.KeyCols()):
		n = &ir.Get{Rel: rel, Alias: scan.Alias, Path: best.path, Key: best.eq, Cols: scan.Cols}
	default:
		n = &ir.Scan{Rel: rel, Alias: scan.Alias, Path: best.path, Eq: best.eq, Lo: best.lo, Hi: best.hi,
			Reverse: best.reverse, Cols: scan.Cols}
	}
	return filtered(n, residual), nil
}

// keyed builds a candidate that binds an equality prefix of p's key and
// optionally a range on the next key column.
func keyed(p *schema.AccessPath, as []atom, rel *schema.Relation, alias string, order []ir.SortKey) *candidate {
	c := &candidate{path: p}
	keyCols := p.KeyCols()
	for _, kc := range keyCols {
		i := slices.IndexFunc(as, func(a atom) bool { return a.col == kc && a.op == ir.OpEq && !slices.Contains(c.used, a.idx) })
		if i < 0 {
			break
		}
		c.eq = append(c.eq, as[i].rhs)
		c.used = append(c.used, as[i].idx)
	}
	if len(c.eq) < len(p.Partition) {
		return nil
	}
	full := len(c.eq) == len(keyCols)
	if !full {
		next := keyCols[len(c.eq)]
		for _, a := range as {
			if a.col != next || slices.Contains(c.used, a.idx) {
				continue
			}
			switch {
			case c.lo == nil && (a.op == ir.OpGt || a.op == ir.OpGe):
				c.lo = &ir.Bound{Expr: a.rhs, Inclusive: a.op == ir.OpGe}
			case c.hi == nil && (a.op == ir.OpLt || a.op == ir.OpLe):
				c.hi = &ir.Bound{Expr: a.rhs, Inclusive: a.op == ir.OpLe}
			default:
				continue
			}
			c.used = append(c.used, a.idx)
		}
	}
	switch {
	case len(c.eq) == 0 && c.lo == nil && c.hi == nil:
		return nil // that is a full scan
	case full && p.Unique && p.Primary:
		c.cost = costPrimaryKey
	case full && p.Unique:
		c.cost = costUniqueKey
	case len(c.eq) > 0:
		c.cost = costPrefix
	default:
		c.cost = costRange
	}
	if order != nil && !(full && p.Unique) {
		rev, ok := satisfies(rel, alias, p, len(c.eq), order)
		if !ok {
			return nil
		}
		c.reverse = rev
	}
	return c
}

// multiGet builds a GetMany candidate from "col IN (...)" on a
// single-column unique path.
func multiGet(p *schema.AccessPath, as []atom, order []ir.SortKey) *candidate {
	keyCols := p.KeyCols()
	if !p.Unique || len(keyCols) != 1 {
		return nil
	}
	for _, a := range as {
		if a.list != nil && a.col == keyCols[0] && (order == nil || len(a.list) == 1) {
			return &candidate{path: p, in: a.list, used: []int{a.idx}, cost: costMultiGet}
		}
	}
	return nil
}

func fullScan(p *schema.AccessPath, rel *schema.Relation, alias string, order []ir.SortKey) *candidate {
	c := &candidate{path: p, cost: costFullScan}
	if order != nil {
		rev, ok := satisfies(rel, alias, p, 0, order)
		if !ok {
			return nil
		}
		c.reverse = rev
	}
	return c
}

// satisfies reports whether scanning p with nEq equality-bound key columns
// yields rows in the given order, and whether the scan must be reversed.
func satisfies(rel *schema.Relation, alias string, p *schema.AccessPath, nEq int, order []ir.SortKey) (reverse, ok bool) {
	keyCols := p.KeyCols()
	pos, dir := nEq, 0 // dir: 0 unset, 1 asc, -1 desc
	for _, k := range order {
		if !owns(k.Col, rel, alias) {
			return false, false
		}
		col := rel.Column(k.Col.Name).ID
		if slices.Contains(keyCols[:nEq], col) {
			continue // constant within the scan
		}
		if pos >= len(keyCols) {
			// A full unique key orders rows completely; later keys are moot.
			if p.Unique {
				break
			}
			return false, false
		}
		if keyCols[pos] != col {
			return false, false
		}
		pos++
		d := 1
		if k.Desc {
			d = -1
		}
		if dir != 0 && d != dir {
			return false, false
		}
		dir = d
	}
	return dir == -1, true
}

func sortString(keys []ir.SortKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k.Col.String()
		if k.Desc {
			parts[i] += " desc"
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
