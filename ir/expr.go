package ir

import (
	"fmt"
	"strings"

	"github.com/fpt/go-dquery/value"
)

// Expr is a scalar expression.
type Expr interface {
	String() string
	expr()
}

// Lit is a literal value.
type Lit struct{ V value.Value }

// Param is a positional parameter, 1-based ($1, $2, ...).
type Param struct{ N int }

// Col references a column of the current row. Qual may be empty when the
// name is unambiguous.
type Col struct{ Qual, Name string }

// Outer references a column of the enclosing row inside a Map subplan.
type Outer struct{ Qual, Name string }

type CmpOp uint8

const (
	OpEq CmpOp = iota + 1
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe
)

var cmpNames = map[CmpOp]string{OpEq: "=", OpNe: "<>", OpLt: "<", OpLe: "<=", OpGt: ">", OpGe: ">="}

func (o CmpOp) String() string { return cmpNames[o] }

// Flip returns the operator with operands swapped (a < b  <=>  b > a).
func (o CmpOp) Flip() CmpOp {
	switch o {
	case OpLt:
		return OpGt
	case OpLe:
		return OpGe
	case OpGt:
		return OpLt
	case OpGe:
		return OpLe
	}
	return o
}

type Cmp struct {
	Op   CmpOp
	L, R Expr
}

// And is a conjunction of its terms (SQL three-valued logic).
type And struct{ Terms []Expr }

// Or is a disjunction of its terms (SQL three-valued logic).
type Or struct{ Terms []Expr }

type Not struct{ X Expr }

type In struct {
	X    Expr
	List []Expr
}

// IsNull is X IS NULL, or X IS NOT NULL when Negate is set.
type IsNull struct {
	X      Expr
	Negate bool
}

type ArithOp uint8

const (
	OpAdd ArithOp = iota + 1
	OpSub
	OpMul
	OpDiv
	OpMod
)

var arithNames = map[ArithOp]string{OpAdd: "+", OpSub: "-", OpMul: "*", OpDiv: "/", OpMod: "%"}

func (o ArithOp) String() string { return arithNames[o] }

type Arith struct {
	Op   ArithOp
	L, R Expr
}

func (Lit) expr()    {}
func (Param) expr()  {}
func (Col) expr()    {}
func (Outer) expr()  {}
func (Cmp) expr()    {}
func (And) expr()    {}
func (Or) expr()     {}
func (Not) expr()    {}
func (In) expr()     {}
func (IsNull) expr() {}
func (Arith) expr()  {}

func (e Lit) String() string   { return e.V.String() }
func (e Param) String() string { return fmt.Sprintf("$%d", e.N) }
func (e Col) String() string   { return qualName(e.Qual, e.Name) }
func (e Outer) String() string { return "$." + qualName(e.Qual, e.Name) }
func (e Cmp) String() string   { return fmt.Sprintf("%s %s %s", wrap(e.L), e.Op, wrap(e.R)) }
func (e And) String() string   { return joinTerms(e.Terms, " AND ") }
func (e Or) String() string    { return joinTerms(e.Terms, " OR ") }
func (e Not) String() string   { return "NOT " + wrap(e.X) }
func (e In) String() string {
	return fmt.Sprintf("%s IN (%s)", wrap(e.X), strings.TrimSuffix(strings.TrimPrefix(joinExprs(e.List), "["), "]"))
}
func (e IsNull) String() string {
	if e.Negate {
		return wrap(e.X) + " IS NOT NULL"
	}
	return wrap(e.X) + " IS NULL"
}
func (e Arith) String() string { return fmt.Sprintf("%s %s %s", wrap(e.L), e.Op, wrap(e.R)) }

func qualName(q, n string) string {
	if q == "" {
		return n
	}
	return q + "." + n
}

// wrap parenthesizes compound expressions.
func wrap(e Expr) string {
	switch e.(type) {
	case Lit, Param, Col, Outer:
		return e.String()
	}
	return "(" + e.String() + ")"
}

func joinTerms(ts []Expr, sep string) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = wrap(t)
	}
	return strings.Join(parts, sep)
}

func joinExprs(es []Expr) string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Constructors for building plans by hand.

func C(qual, name string) Col          { return Col{Qual: qual, Name: name} }
func O(qual, name string) Outer        { return Outer{Qual: qual, Name: name} }
func P(n int) Param                    { return Param{N: n} }
func V(v value.Value) Lit              { return Lit{V: v} }
func I(i int64) Lit                    { return Lit{V: value.Int(i)} }
func F(f float64) Lit                  { return Lit{V: value.Float(f)} }
func S(s string) Lit                   { return Lit{V: value.String(s)} }
func Eq(l, r Expr) Cmp                 { return Cmp{Op: OpEq, L: l, R: r} }
func Ne(l, r Expr) Cmp                 { return Cmp{Op: OpNe, L: l, R: r} }
func Lt(l, r Expr) Cmp                 { return Cmp{Op: OpLt, L: l, R: r} }
func Le(l, r Expr) Cmp                 { return Cmp{Op: OpLe, L: l, R: r} }
func Gt(l, r Expr) Cmp                 { return Cmp{Op: OpGt, L: l, R: r} }
func Ge(l, r Expr) Cmp                 { return Cmp{Op: OpGe, L: l, R: r} }
func AndOf(terms ...Expr) And          { return And{Terms: terms} }
func OrOf(terms ...Expr) Or            { return Or{Terms: terms} }
func Add(l, r Expr) Arith              { return Arith{Op: OpAdd, L: l, R: r} }
func Exprs(es ...Expr) []Expr          { return es }
func As(name string, e Expr) NamedExpr { return NamedExpr{Name: name, Expr: e} }
