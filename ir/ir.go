// Package ir defines the query IR: a DAG of read and mutation operators over
// access paths, independent of any frontend language or storage engine.
//
// Row layout: Get, GetMany and Scan produce full-width rows of their relation
// (columns qualified by Alias). Lookup appends the target relation's columns
// to its input row. Map appends one value per field. Project replaces the
// layout. Filter and Limit preserve it.
package ir

import "github.com/fpt/go-dquery/schema"

// Node is a plan operator.
type Node interface {
	Shape() Shape
	Inputs() []Node
	node()
}

// Shape describes a node's output cardinality.
type Shape uint8

const (
	ShapeOne    Shape = iota + 1 // 0..1 rows
	ShapeStream                  // any number of rows
	ShapeEffect                  // mutation result
)

func (s Shape) String() string {
	switch s {
	case ShapeOne:
		return "one"
	case ShapeStream:
		return "stream"
	case ShapeEffect:
		return "effect"
	}
	return "?"
}

// Bound is one end of a range on a key column.
type Bound struct {
	Expr      Expr
	Inclusive bool
}

// NamedExpr is an output column definition.
type NamedExpr struct {
	Name string
	Expr Expr
}

// Assign is a SET clause item.
type Assign struct {
	Col  string
	Expr Expr
}

// Get reads at most one row by a full key on a unique path.
type Get struct {
	Rel   *schema.Relation
	Alias string
	Path  *schema.AccessPath
	Key   []Expr
	Cols  []schema.ColID // nil = all
}

// GetMany reads rows by a list of full keys on a unique path, in key order.
// Missing keys produce no row.
type GetMany struct {
	Rel   *schema.Relation
	Alias string
	Path  *schema.AccessPath
	Keys  [][]Expr
	Cols  []schema.ColID
}

// Scan reads rows in path order. Eq binds a key prefix; Lo/Hi bound the next
// key column. No Eq and no bounds means a full scan of the path.
type Scan struct {
	Rel     *schema.Relation
	Alias   string
	Path    *schema.AccessPath
	Eq      []Expr
	Lo, Hi  *Bound
	Reverse bool
	Limit   int // 0 = none
	Cols    []schema.ColID
}

type Filter struct {
	Input Node
	Pred  Expr
}

type Project struct {
	Input Node
	Exprs []NamedExpr
}

// Lookup traverses from each input row to rows of Rel through Path, with Key
// evaluated against the input row. Each match produces input ++ target. With
// Many unset the path must be unique and Key must be a full key (N:1 / 1:1).
// Optional keeps input rows without a match, padding with NULLs (left join).
type Lookup struct {
	Input    Node
	Rel      *schema.Relation
	Alias    string
	Path     *schema.AccessPath
	Key      []Expr
	Many     bool
	Optional bool
	Reverse  bool
	Limit    int // per input row; 0 = none
	Cols     []schema.ColID
}

// MapField is a nested subplan evaluated once per input row. Inside Plan,
// Outer expressions refer to the input row. With One set the field is a
// record (or NULL); otherwise it is a list of records.
//
// Batched asks the executor to fetch the subplan's leaf access for a whole
// input batch at once (one GetMany, or parallel scans) instead of once per
// row. It requires Plan to be a chain of row-wise operators over a single
// Get or Scan leaf (see BatchLeaf).
type MapField struct {
	Name    string
	Plan    Node
	One     bool
	Batched bool
}

// SortKey is one ORDER BY item.
type SortKey struct {
	Col  Col
	Desc bool
}

// Sort is a logical ordering requirement. It has no runtime implementation:
// the optimizer must satisfy it with an access path order and remove it.
type Sort struct {
	Input Node
	Keys  []SortKey
}

// Map appends nested results to each input row.
type Map struct {
	Input  Node
	Fields []MapField
}

type Limit struct {
	Input Node
	N     int
}

// Return marks the root of a plan.
type Return struct{ Input Node }

// OnConflict controls INSERT behavior on a primary key conflict. Set
// expressions see the existing row under the relation alias and the proposed
// row under the qualifier "excluded".
type OnConflict struct {
	DoNothing bool
	Set       []Assign
}

// Insert inserts either literal Rows or the rows of Input, positionally
// mapped to Cols (nil = all columns in order).
type Insert struct {
	Rel        *schema.Relation
	Alias      string
	Cols       []string
	Rows       [][]Expr
	Input      Node
	OnConflict *OnConflict
	Returning  []NamedExpr
}

// Update changes the rows of Rel produced by Input. Input must contain all
// columns of Rel under Alias; Set expressions are evaluated against the
// input row. Returning sees the new row.
type Update struct {
	Rel       *schema.Relation
	Alias     string
	Input     Node
	Set       []Assign
	Returning []NamedExpr
}

// Delete deletes the rows of Rel produced by Input. Returning sees the old
// row.
type Delete struct {
	Rel       *schema.Relation
	Alias     string
	Input     Node
	Returning []NamedExpr
}

func (*Get) Shape() Shape      { return ShapeOne }
func (*GetMany) Shape() Shape  { return ShapeStream }
func (*Scan) Shape() Shape     { return ShapeStream }
func (n *Filter) Shape() Shape { return n.Input.Shape() }
func (n *Project) Shape() Shape {
	return n.Input.Shape()
}
func (n *Lookup) Shape() Shape {
	if !n.Many && n.Input.Shape() == ShapeOne {
		return ShapeOne
	}
	return ShapeStream
}
func (n *Map) Shape() Shape    { return n.Input.Shape() }
func (n *Sort) Shape() Shape   { return n.Input.Shape() }
func (n *Limit) Shape() Shape  { return n.Input.Shape() }
func (n *Return) Shape() Shape { return n.Input.Shape() }
func (*Insert) Shape() Shape   { return ShapeEffect }
func (*Update) Shape() Shape   { return ShapeEffect }
func (*Delete) Shape() Shape   { return ShapeEffect }

func (*Get) Inputs() []Node       { return nil }
func (*GetMany) Inputs() []Node   { return nil }
func (*Scan) Inputs() []Node      { return nil }
func (n *Filter) Inputs() []Node  { return []Node{n.Input} }
func (n *Project) Inputs() []Node { return []Node{n.Input} }
func (n *Lookup) Inputs() []Node  { return []Node{n.Input} }
func (n *Map) Inputs() []Node {
	in := []Node{n.Input}
	for _, f := range n.Fields {
		in = append(in, f.Plan)
	}
	return in
}
func (n *Limit) Inputs() []Node  { return []Node{n.Input} }
func (n *Sort) Inputs() []Node   { return []Node{n.Input} }
func (n *Return) Inputs() []Node { return []Node{n.Input} }
func (n *Insert) Inputs() []Node {
	if n.Input != nil {
		return []Node{n.Input}
	}
	return nil
}
func (n *Update) Inputs() []Node { return []Node{n.Input} }
func (n *Delete) Inputs() []Node { return []Node{n.Input} }

func (*Get) node()     {}
func (*GetMany) node() {}
func (*Scan) node()    {}
func (*Filter) node()  {}
func (*Project) node() {}
func (*Lookup) node()  {}
func (*Map) node()     {}
func (*Limit) node()   {}
func (*Sort) node()    {}
func (*Return) node()  {}
func (*Insert) node()  {}
func (*Update) node()  {}
func (*Delete) node()  {}

// AliasOr returns alias, or the relation name if alias is empty.
func AliasOr(alias string, rel *schema.Relation) string {
	if alias == "" {
		return rel.Name
	}
	return alias
}

// BatchLeaf returns the single Get or Scan leaf of a chain of row-wise
// operators (Filter, Project, Limit, Lookup, Map), or nil if plan has another
// shape. Such a plan can be executed for many outer rows by fetching the leaf
// in one batch.
func BatchLeaf(plan Node) Node {
	for {
		switch n := plan.(type) {
		case *Get, *Scan:
			return n
		case *Filter:
			plan = n.Input
		case *Project:
			plan = n.Input
		case *Limit:
			plan = n.Input
		case *Lookup:
			plan = n.Input
		case *Map:
			plan = n.Input
		default:
			return nil
		}
	}
}
