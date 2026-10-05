package sql

import "github.com/fpt/go-dquery/ir"

// Statement is a parsed SQL statement. Expressions are parsed directly into
// ir.Expr; column references are unresolved until binding.
type Statement interface{ stmt() }

type TableRef struct {
	Name  string
	Alias string // empty if none
	Pos   int
}

// Ref returns the name the table is referred to by.
func (t TableRef) Ref() string {
	if t.Alias != "" {
		return t.Alias
	}
	return t.Name
}

// SelectItem is "*", "q.*" or "expr [AS alias]".
type SelectItem struct {
	Star  bool
	Qual  string // for q.*
	Expr  ir.Expr
	Alias string
}

type Join struct {
	Left  bool
	Table TableRef
	On    ir.Expr
}

type OrderItem struct {
	Expr ir.Expr
	Desc bool
}

type Select struct {
	Items   []SelectItem
	From    TableRef
	Joins   []Join
	Where   ir.Expr
	OrderBy []OrderItem
	Limit   int // -1 = none
}

type Conflict struct {
	Target    []string
	DoNothing bool
	Set       []ir.Assign
}

type Insert struct {
	Table     TableRef
	Cols      []string
	Rows      [][]ir.Expr
	Conflict  *Conflict
	Returning []SelectItem
}

type Update struct {
	Table     TableRef
	Set       []ir.Assign
	Where     ir.Expr
	Returning []SelectItem
}

type Delete struct {
	Table     TableRef
	Where     ir.Expr
	Returning []SelectItem
}

// Explain wraps a statement whose plan should be shown instead of run.
type Explain struct{ Stmt Statement }

func (*Select) stmt()  {}
func (*Insert) stmt()  {}
func (*Update) stmt()  {}
func (*Delete) stmt()  {}
func (*Explain) stmt() {}
