package ir

import (
	"fmt"
	"strings"

	"github.com/fpt/go-dquery/schema"
)

// Explain renders a plan as an indented tree, one operator per line. The
// output is deterministic and used by golden tests.
func Explain(n Node) string {
	var b strings.Builder
	explain(&b, n, 0)
	return b.String()
}

func explain(b *strings.Builder, n Node, depth int) {
	indent := strings.Repeat("  ", depth)
	b.WriteString(indent)
	b.WriteString(line(n))
	b.WriteByte('\n')
	if m, ok := n.(*Map); ok {
		explain(b, m.Input, depth+1)
		for _, f := range m.Fields {
			card := "many"
			if f.One {
				card = "one"
			}
			if f.Batched {
				card += ", batched"
			}
			fmt.Fprintf(b, "%s  field %s (%s)\n", indent, f.Name, card)
			explain(b, f.Plan, depth+2)
		}
		return
	}
	for _, in := range n.Inputs() {
		explain(b, in, depth+1)
	}
}

func line(n Node) string {
	var w words
	switch n := n.(type) {
	case *Get:
		w.add("Get", relRef(n.Rel, n.Alias), "via", n.Path.Name, "key="+joinExprs(n.Key))
		w.cols(n.Rel, n.Cols)
	case *GetMany:
		keys := make([]string, len(n.Keys))
		for i, k := range n.Keys {
			keys[i] = joinExprs(k)
		}
		w.add("GetMany", relRef(n.Rel, n.Alias), "via", n.Path.Name, "keys=["+strings.Join(keys, ", ")+"]")
		w.cols(n.Rel, n.Cols)
	case *Scan:
		w.add("Scan", relRef(n.Rel, n.Alias), "via", n.Path.Name)
		if len(n.Eq) > 0 {
			w.add("eq=" + joinExprs(n.Eq))
		}
		w.bound("lo", n.Lo)
		w.bound("hi", n.Hi)
		if n.Reverse {
			w.add("reverse")
		}
		if n.Limit > 0 {
			w.add(fmt.Sprintf("limit=%d", n.Limit))
		}
		w.cols(n.Rel, n.Cols)
	case *Filter:
		w.add("Filter", n.Pred.String())
	case *Project:
		w.add("Project", namedExprs(n.Exprs))
	case *Lookup:
		w.add("Lookup", relRef(n.Rel, n.Alias), "via", n.Path.Name, "key="+joinExprs(n.Key))
		if n.Many {
			w.add("many")
		} else {
			w.add("one")
		}
		if n.Optional {
			w.add("optional")
		}
		if n.Reverse {
			w.add("reverse")
		}
		if n.Limit > 0 {
			w.add(fmt.Sprintf("limit=%d", n.Limit))
		}
		w.cols(n.Rel, n.Cols)
	case *Map:
		w.add("Map")
	case *Limit:
		w.add("Limit", fmt.Sprint(n.N))
	case *Sort:
		keys := make([]string, len(n.Keys))
		for i, k := range n.Keys {
			keys[i] = k.Col.String()
			if k.Desc {
				keys[i] += " desc"
			}
		}
		w.add("Sort", "["+strings.Join(keys, ", ")+"]")
	case *Return:
		w.add("Return")
	case *Insert:
		w.add("Insert", relRef(n.Rel, n.Alias))
		if n.Cols != nil {
			w.add("cols=[" + strings.Join(n.Cols, ", ") + "]")
		}
		for _, r := range n.Rows {
			w.add("values=" + joinExprs(r))
		}
		if c := n.OnConflict; c != nil {
			if c.DoNothing {
				w.add("on-conflict=nothing")
			} else {
				w.add("on-conflict=update", assigns(c.Set))
			}
		}
		w.returning(n.Returning)
	case *Update:
		w.add("Update", relRef(n.Rel, n.Alias), "set", assigns(n.Set))
		w.returning(n.Returning)
	case *Delete:
		w.add("Delete", relRef(n.Rel, n.Alias))
		w.returning(n.Returning)
	default:
		w.add(fmt.Sprintf("%T", n))
	}
	return strings.Join(w, " ")
}

type words []string

func (w *words) add(s ...string) { *w = append(*w, s...) }

func (w *words) bound(name string, b *Bound) {
	if b == nil {
		return
	}
	op := ">"
	if name == "hi" {
		op = "<"
	}
	if b.Inclusive {
		op += "="
	}
	w.add(name + op + b.Expr.String())
}

func (w *words) cols(rel *schema.Relation, cols []schema.ColID) {
	if cols == nil {
		return
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = rel.Columns[c].Name
	}
	w.add("cols=[" + strings.Join(names, ", ") + "]")
}

func (w *words) returning(es []NamedExpr) {
	if es != nil {
		w.add("returning", namedExprs(es))
	}
}

func relRef(rel *schema.Relation, alias string) string {
	if alias == "" || alias == rel.Name {
		return rel.Name
	}
	return rel.Name + " " + alias
}

func namedExprs(es []NamedExpr) string {
	parts := make([]string, len(es))
	for i, e := range es {
		s := e.Expr.String()
		if c, ok := e.Expr.(Col); !ok || c.Name != e.Name {
			s = e.Name + ": " + s
		}
		parts[i] = s
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func assigns(as []Assign) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = a.Col + " = " + a.Expr.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
