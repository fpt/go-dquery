package graphql

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/validator"

	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/opt"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/value"
)

// ErrNotSupported reports GraphQL features outside the supported subset.
var ErrNotSupported = errors.New("graphql: not supported")

// Request is a GraphQL request as sent over HTTP.
type Request struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

// Operation is a compiled operation: one plan per root field, executed in
// order.
type Operation struct {
	Mutation bool
	Fields   []*RootField
}

// RootField is one top-level field of an operation.
type RootField struct {
	Key     string  // response key
	Logical ir.Node // nil for __typename
	Plan    ir.Node // optimized
	kind    rootKind
	name    string     // __typename value for constant fields
	resp    []respItem // mutation response layout
}

type respItem struct {
	key  string
	what string // affected_rows, returning or __typename
	name string // __typename value
}

const typenameKind rootKind = 0

// Compile parses, validates and lowers req into optimized plans.
func (s *Schema) Compile(req Request, o opt.Options) (*Operation, gqlerror.List) {
	doc, errs := gqlparser.LoadQuery(s.ast, req.Query)
	if len(errs) > 0 {
		return nil, errs
	}
	var op *ast.OperationDefinition
	switch {
	case req.OperationName != "":
		op = doc.Operations.ForName(req.OperationName)
	case len(doc.Operations) == 1:
		op = doc.Operations[0]
	default:
		return nil, gqlerror.List{gqlerror.Errorf("operationName is required when the document has several operations")}
	}
	if op == nil {
		return nil, gqlerror.List{gqlerror.Errorf("unknown operation %q", req.OperationName)}
	}
	if op.Operation == ast.Subscription {
		return nil, gqlerror.List{gqlerror.Wrap(fmt.Errorf("%w: subscriptions", ErrNotSupported))}
	}
	vars, err := validator.VariableValues(s.ast, op, req.Variables)
	if err != nil {
		return nil, gqlerror.List{gqlerror.WrapIfUnwrapped(err)}
	}
	l := &lowerer{s: s, vars: vars}
	out := &Operation{Mutation: op.Operation == ast.Mutation}
	for _, f := range l.collect(op.SelectionSet) {
		rf, err := l.root(f, out.Mutation)
		if err == nil && rf.Logical != nil {
			rf.Plan, err = opt.Optimize(rf.Logical, o)
		}
		if err != nil {
			return nil, gqlerror.List{gqlerror.WrapPath(ast.Path{ast.PathName(key(f))}, err)}
		}
		out.Fields = append(out.Fields, rf)
	}
	return out, nil
}

type lowerer struct {
	s    *Schema
	vars map[string]any
	nmap int // counter for internal Map field names
}

func key(f *ast.Field) string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

// collect flattens fragments and applies @skip/@include, merging fields
// with the same response key in first-appearance order.
func (l *lowerer) collect(set ast.SelectionSet) []*ast.Field {
	var order []string
	byKey := map[string]*ast.Field{}
	var visit func(ast.SelectionSet)
	visit = func(ss ast.SelectionSet) {
		for _, sel := range ss {
			switch x := sel.(type) {
			case *ast.Field:
				if !l.included(x.Directives) {
					continue
				}
				k := key(x)
				if prev, ok := byKey[k]; ok {
					merged := *prev
					merged.SelectionSet = append(slices.Clone(prev.SelectionSet), x.SelectionSet...)
					byKey[k] = &merged
					continue
				}
				byKey[k] = x
				order = append(order, k)
			case *ast.FragmentSpread:
				if l.included(x.Directives) {
					visit(x.Definition.SelectionSet)
				}
			case *ast.InlineFragment:
				if l.included(x.Directives) {
					visit(x.SelectionSet)
				}
			}
		}
	}
	visit(set)
	out := make([]*ast.Field, len(order))
	for i, k := range order {
		out[i] = byKey[k]
	}
	return out
}

func (l *lowerer) included(ds ast.DirectiveList) bool {
	if d := ds.ForName("skip"); d != nil {
		if b, _ := d.ArgumentMap(l.vars)["if"].(bool); b {
			return false
		}
	}
	if d := ds.ForName("include"); d != nil {
		if b, _ := d.ArgumentMap(l.vars)["if"].(bool); !b {
			return false
		}
	}
	return true
}

func (l *lowerer) root(f *ast.Field, mutation bool) (*RootField, error) {
	rf := &RootField{Key: key(f)}
	if f.Name == "__typename" {
		rf.kind, rf.name = typenameKind, "Query"
		if mutation {
			rf.name = "Mutation"
		}
		return rf, nil
	}
	if f.Name == "__schema" || f.Name == "__type" {
		return nil, fmt.Errorf("%w: introspection (%s); fetch the SDL instead", ErrNotSupported, f.Name)
	}
	def, ok := l.s.roots[f.Name]
	if !ok {
		return nil, fmt.Errorf("graphql: unknown root field %q", f.Name)
	}
	rf.kind = def.kind
	rel, alias := def.rel, def.rel.Name
	args := f.ArgumentMap(l.vars)
	var err error
	switch def.kind {
	case rootList:
		rf.Logical, err = l.list(rel, alias, args, f.SelectionSet, nil)
	case rootByKey:
		var conj []ir.Expr
		for _, id := range def.path.KeyCols() {
			c := rel.Columns[id]
			v, err := toValue(args[c.Name], c)
			if err != nil {
				return nil, err
			}
			conj = append(conj, ir.Eq(ir.C(alias, c.Name), ir.V(v)))
		}
		rf.Logical, err = l.selection(filter(scan(rel, alias), conj), rel, alias, f.SelectionSet)
	case rootInsert, rootUpdate, rootDelete:
		rf.Logical, rf.resp, err = l.mutation(def, args, f.SelectionSet)
	}
	return rf, err
}

func scan(rel *schema.Relation, alias string) *ir.Scan {
	return &ir.Scan{Rel: rel, Alias: alias, Path: rel.Primary}
}

func filter(n ir.Node, conj []ir.Expr) ir.Node {
	switch len(conj) {
	case 0:
		return n
	case 1:
		return &ir.Filter{Input: n, Pred: conj[0]}
	}
	return &ir.Filter{Input: n, Pred: ir.And{Terms: conj}}
}

// list lowers a list field: Filter (where + relationship link), Sort
// (order_by), Limit, then the selection.
func (l *lowerer) list(rel *schema.Relation, alias string, args map[string]any, sel ast.SelectionSet, conj []ir.Expr) (ir.Node, error) {
	if w := args["where"]; w != nil {
		e, err := l.boolExp(w, rel, alias)
		if err != nil {
			return nil, err
		}
		if e != nil {
			conj = append(conj, e)
		}
	}
	n := filter(scan(rel, alias), conj)
	if ob := args["order_by"]; ob != nil {
		keys, err := orderBy(ob, rel, alias)
		if err != nil {
			return nil, err
		}
		if len(keys) > 0 {
			n = &ir.Sort{Input: n, Keys: keys}
		}
	}
	if lim := args["limit"]; lim != nil {
		k, err := toInt(lim)
		if err != nil || k < 0 {
			return nil, fmt.Errorf("graphql: limit must be a non-negative integer")
		}
		n = &ir.Limit{Input: n, N: int(k)}
	}
	return l.selection(n, rel, alias, sel)
}

// selection projects the selected fields. Relationship fields become Map
// fields whose subplans refer to this row through Outer references.
func (l *lowerer) selection(n ir.Node, rel *schema.Relation, alias string, sel ast.SelectionSet) (ir.Node, error) {
	var items []ir.NamedExpr
	var fields []ir.MapField
	for _, f := range l.collect(sel) {
		k := key(f)
		switch {
		case f.Name == "__typename":
			items = append(items, ir.As(k, ir.S(TypeName(rel))))
		case rel.Column(f.Name) != nil:
			items = append(items, ir.As(k, ir.C(alias, f.Name)))
		default:
			rs := l.s.cat.Relationship(rel, f.Name)
			if rs == nil {
				return nil, fmt.Errorf("graphql: %s has no field %q", TypeName(rel), f.Name)
			}
			sub, err := l.related(rs, alias, f)
			if err != nil {
				return nil, err
			}
			name := fmt.Sprintf("#%d", l.nmap)
			l.nmap++
			fields = append(fields, ir.MapField{Name: name, Plan: sub, One: rs.Kind != schema.OneToMany})
			items = append(items, ir.As(k, ir.C("", name)))
		}
	}
	if len(fields) > 0 {
		n = &ir.Map{Input: n, Fields: fields}
	}
	return &ir.Project{Input: n, Exprs: items}, nil
}

// related lowers a relationship field: the target's key columns are bound
// to the parent row's columns.
func (l *lowerer) related(rs *schema.Relationship, parent string, f *ast.Field) (ir.Node, error) {
	to, alias := rs.To, rs.To.Name
	keyCols := rs.Path.KeyCols()
	var conj []ir.Expr
	for i, fc := range rs.FromCols {
		conj = append(conj, ir.Eq(ir.C(alias, to.Columns[keyCols[i]].Name), ir.O(parent, rs.From.Columns[fc].Name)))
	}
	if rs.Kind == schema.OneToMany {
		return l.list(to, alias, f.ArgumentMap(l.vars), f.SelectionSet, conj)
	}
	return l.selection(filter(scan(to, alias), conj), to, alias, f.SelectionSet)
}

// boolExp lowers a <T>_bool_exp object. Keys are processed in sorted order
// so plans are deterministic. It returns nil for an empty object.
func (l *lowerer) boolExp(v any, rel *schema.Relation, alias string) (ir.Expr, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("graphql: where must be an object")
	}
	var terms []ir.Expr
	for _, k := range sortedKeys(m) {
		arg := m[k]
		switch k {
		case "_and", "_or":
			list, _ := arg.([]any)
			var sub []ir.Expr
			for _, x := range list {
				e, err := l.boolExp(x, rel, alias)
				if err != nil {
					return nil, err
				}
				if e != nil {
					sub = append(sub, e)
				}
			}
			switch {
			case len(sub) == 0 && k == "_or":
				terms = append(terms, ir.V(value.Bool(false))) // empty disjunction
			case len(sub) == 0:
			case len(sub) == 1:
				terms = append(terms, sub[0])
			case k == "_and":
				terms = append(terms, ir.And{Terms: sub})
			default:
				terms = append(terms, ir.Or{Terms: sub})
			}
		case "_not":
			e, err := l.boolExp(arg, rel, alias)
			if err != nil {
				return nil, err
			}
			if e != nil {
				terms = append(terms, ir.Not{X: e})
			} else {
				terms = append(terms, ir.V(value.Bool(false)))
			}
		default:
			col := rel.Column(k)
			if col == nil {
				return nil, fmt.Errorf("graphql: %s has no column %q", rel.Name, k)
			}
			es, err := comparison(arg, col, alias)
			if err != nil {
				return nil, err
			}
			terms = append(terms, es...)
		}
	}
	switch len(terms) {
	case 0:
		return nil, nil
	case 1:
		return terms[0], nil
	}
	return ir.And{Terms: terms}, nil
}

var cmpOps = map[string]ir.CmpOp{"_eq": ir.OpEq, "_neq": ir.OpNe, "_gt": ir.OpGt, "_gte": ir.OpGe, "_lt": ir.OpLt, "_lte": ir.OpLe}

func comparison(v any, col *schema.Column, alias string) ([]ir.Expr, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("graphql: comparison for %s must be an object", col.Name)
	}
	c := ir.C(alias, col.Name)
	var out []ir.Expr
	for _, op := range sortedKeys(m) {
		arg := m[op]
		switch op {
		case "_in":
			list, _ := arg.([]any)
			var es []ir.Expr
			for _, x := range list {
				val, err := toValue(x, col)
				if err != nil {
					return nil, err
				}
				es = append(es, ir.V(val))
			}
			if len(es) == 0 {
				out = append(out, ir.V(value.Bool(false)))
				continue
			}
			out = append(out, ir.In{X: c, List: es})
		case "_is_null":
			b, ok := arg.(bool)
			if !ok {
				return nil, fmt.Errorf("graphql: _is_null must be a boolean")
			}
			out = append(out, ir.IsNull{X: c, Negate: !b})
		default:
			cop, ok := cmpOps[op]
			if !ok {
				return nil, fmt.Errorf("graphql: unknown comparison %q", op)
			}
			val, err := toValue(arg, col)
			if err != nil {
				return nil, err
			}
			out = append(out, ir.Cmp{Op: cop, L: c, R: ir.V(val)})
		}
	}
	return out, nil
}

// orderBy lowers [<T>_order_by!]. Within one object, keys follow column
// order; use a list of single-key objects to control precedence.
func orderBy(v any, rel *schema.Relation, alias string) ([]ir.SortKey, error) {
	items, ok := v.([]any)
	if !ok {
		items = []any{v} // list input coercion of a single value
	}
	var keys []ir.SortKey
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("graphql: order_by items must be objects")
		}
		for _, c := range rel.Columns {
			dir, ok := m[c.Name]
			if !ok || dir == nil {
				continue
			}
			keys = append(keys, ir.SortKey{Col: ir.C(alias, c.Name), Desc: dir == "desc"})
		}
	}
	return keys, nil
}

func (l *lowerer) mutation(def rootDef, args map[string]any, sel ast.SelectionSet) (ir.Node, []respItem, error) {
	rel, alias := def.rel, def.rel.Name
	var resp []respItem
	var returning []ir.NamedExpr
	for _, f := range l.collect(sel) {
		item := respItem{key: key(f), what: f.Name}
		switch f.Name {
		case "__typename":
			item.name = TypeName(rel) + "_mutation_response"
		case "returning":
			if returning != nil {
				return nil, nil, fmt.Errorf("%w: selecting returning more than once", ErrNotSupported)
			}
			returning = []ir.NamedExpr{}
			for _, rf := range l.collect(f.SelectionSet) {
				switch {
				case rf.Name == "__typename":
					returning = append(returning, ir.As(key(rf), ir.S(TypeName(rel))))
				case rel.Column(rf.Name) != nil:
					returning = append(returning, ir.As(key(rf), ir.C(alias, rf.Name)))
				default:
					return nil, nil, fmt.Errorf("%w: relationship %q in returning", ErrNotSupported, rf.Name)
				}
			}
		}
		resp = append(resp, item)
	}

	where := func() (ir.Node, error) {
		e, err := l.boolExp(args["where"], rel, alias)
		if err != nil {
			return nil, err
		}
		var conj []ir.Expr
		if e != nil {
			conj = append(conj, e)
		}
		return filter(scan(rel, alias), conj), nil
	}

	switch def.kind {
	case rootInsert:
		objs, _ := args["objects"].([]any)
		n := &ir.Insert{Rel: rel, Alias: alias, Returning: returning}
		for _, o := range objs {
			m, _ := o.(map[string]any)
			row := make([]ir.Expr, len(rel.Columns))
			for i, c := range rel.Columns {
				v, err := toValue(m[c.Name], c)
				if err != nil {
					return nil, nil, err
				}
				row[i] = ir.V(v)
			}
			n.Rows = append(n.Rows, row)
		}
		if len(n.Rows) == 0 {
			n.Rows = [][]ir.Expr{}
		}
		return n, resp, nil
	case rootUpdate:
		input, err := where()
		if err != nil {
			return nil, nil, err
		}
		set, _ := args["_set"].(map[string]any)
		n := &ir.Update{Rel: rel, Alias: alias, Input: input, Returning: returning}
		for _, c := range rel.Columns {
			if raw, ok := set[c.Name]; ok {
				v, err := toValue(raw, c)
				if err != nil {
					return nil, nil, err
				}
				n.Set = append(n.Set, ir.Assign{Col: c.Name, Expr: ir.V(v)})
			}
		}
		return n, resp, nil
	default:
		input, err := where()
		if err != nil {
			return nil, nil, err
		}
		return &ir.Delete{Rel: rel, Alias: alias, Input: input, Returning: returning}, resp, nil
	}
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// toValue converts a GraphQL input value (literal or JSON variable) to a
// value of the column's type.
func toValue(x any, col *schema.Column) (value.Value, error) {
	if x == nil {
		return value.Null, nil
	}
	bad := func() (value.Value, error) {
		return value.Null, fmt.Errorf("graphql: %s: cannot use %v (%T) as %s", col.Name, x, x, col.Type)
	}
	switch col.Type {
	case schema.TypeInt:
		i, err := toInt(x)
		if err != nil {
			return bad()
		}
		return value.Int(i), nil
	case schema.TypeFloat:
		f, err := toFloat(x)
		if err != nil {
			return bad()
		}
		return value.Float(f), nil
	case schema.TypeBool:
		if b, ok := x.(bool); ok {
			return value.Bool(b), nil
		}
	case schema.TypeString:
		if s, ok := x.(string); ok {
			return value.String(s), nil
		}
	case schema.TypeTimestamp:
		if s, ok := x.(string); ok {
			if t, ok := value.ParseTimestamp(s); ok {
				return t, nil
			}
		}
	case schema.TypeBytes:
		if s, ok := x.(string); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return value.Bytes(b), nil
			}
		}
	}
	return bad()
}

func toInt(x any) (int64, error) {
	switch n := x.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case float64:
		if n == math.Trunc(n) && math.Abs(n) < 1<<53 {
			return int64(n), nil
		}
	case json.Number:
		return n.Int64()
	}
	return 0, fmt.Errorf("not an integer: %v", x)
}

func toFloat(x any) (float64, error) {
	switch n := x.(type) {
	case float64:
		return n, nil
	case int64:
		return float64(n), nil
	case int:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	}
	return 0, fmt.Errorf("not a number: %v", x)
}
