package exec

import (
	"errors"
	"fmt"
	"math"

	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/value"
)

// evalFn evaluates an expression against the current row.
type evalFn func(row value.Row, env *env) (value.Value, error)

// compileExpr resolves column references against cur (the current row) and
// outer (the enclosing Map row; may be nil).
func compileExpr(e ir.Expr, cur, outer *layout) (evalFn, error) {
	switch e := e.(type) {
	case ir.Lit:
		v := e.V
		return func(value.Row, *env) (value.Value, error) { return v, nil }, nil

	case ir.Param:
		n := e.N
		if n < 1 {
			return nil, fmt.Errorf("exec: invalid parameter $%d", n)
		}
		return func(_ value.Row, env *env) (value.Value, error) {
			if n > len(env.params) {
				return value.Null, fmt.Errorf("exec: missing parameter $%d", n)
			}
			return env.params[n-1], nil
		}, nil

	case ir.Col:
		if cur == nil {
			return nil, fmt.Errorf("exec: column %s is not available here", e)
		}
		i, err := cur.resolve(e.Qual, e.Name)
		if err != nil {
			return nil, err
		}
		return func(row value.Row, _ *env) (value.Value, error) { return row[i], nil }, nil

	case ir.Outer:
		if outer == nil {
			return nil, fmt.Errorf("exec: outer reference %s outside of a Map subplan", e)
		}
		i, err := outer.resolve(e.Qual, e.Name)
		if err != nil {
			return nil, err
		}
		return func(_ value.Row, env *env) (value.Value, error) { return env.outer[i], nil }, nil

	case ir.Cmp:
		l, r, err := compile2(e.L, e.R, cur, outer)
		if err != nil {
			return nil, err
		}
		op := e.Op
		return func(row value.Row, env *env) (value.Value, error) {
			a, err := l(row, env)
			if err != nil {
				return value.Null, err
			}
			b, err := r(row, env)
			if err != nil {
				return value.Null, err
			}
			return compareOp(op, a, b)
		}, nil

	case ir.And:
		return compileLogic(e.Terms, true, cur, outer)
	case ir.Or:
		return compileLogic(e.Terms, false, cur, outer)

	case ir.Not:
		x, err := compileExpr(e.X, cur, outer)
		if err != nil {
			return nil, err
		}
		return func(row value.Row, env *env) (value.Value, error) {
			v, err := x(row, env)
			if err != nil || v.IsNull() {
				return value.Null, err
			}
			if v.Kind() != value.KindBool {
				return value.Null, fmt.Errorf("exec: NOT of %s", v.Kind())
			}
			return value.Bool(!v.Bool()), nil
		}, nil

	case ir.In:
		x, err := compileExpr(e.X, cur, outer)
		if err != nil {
			return nil, err
		}
		list := make([]evalFn, len(e.List))
		for i, le := range e.List {
			if list[i], err = compileExpr(le, cur, outer); err != nil {
				return nil, err
			}
		}
		return func(row value.Row, env *env) (value.Value, error) {
			v, err := x(row, env)
			if err != nil || v.IsNull() {
				return value.Null, err
			}
			sawNull := false
			for _, f := range list {
				w, err := f(row, env)
				if err != nil {
					return value.Null, err
				}
				if w.IsNull() {
					sawNull = true
					continue
				}
				c, err := compareValues(v, w)
				if err != nil {
					return value.Null, err
				}
				if c == 0 {
					return value.Bool(true), nil
				}
			}
			if sawNull {
				return value.Null, nil
			}
			return value.Bool(false), nil
		}, nil

	case ir.IsNull:
		x, err := compileExpr(e.X, cur, outer)
		if err != nil {
			return nil, err
		}
		neg := e.Negate
		return func(row value.Row, env *env) (value.Value, error) {
			v, err := x(row, env)
			return value.Bool(v.IsNull() != neg), err
		}, nil

	case ir.Arith:
		l, r, err := compile2(e.L, e.R, cur, outer)
		if err != nil {
			return nil, err
		}
		op := e.Op
		return func(row value.Row, env *env) (value.Value, error) {
			a, err := l(row, env)
			if err != nil {
				return value.Null, err
			}
			b, err := r(row, env)
			if err != nil {
				return value.Null, err
			}
			return arith(op, a, b)
		}, nil
	}
	return nil, fmt.Errorf("exec: unsupported expression %T", e)
}

func compile2(a, b ir.Expr, cur, outer *layout) (evalFn, evalFn, error) {
	l, err := compileExpr(a, cur, outer)
	if err != nil {
		return nil, nil, err
	}
	r, err := compileExpr(b, cur, outer)
	return l, r, err
}

// compileLogic implements three-valued AND (isAnd) and OR.
func compileLogic(terms []ir.Expr, isAnd bool, cur, outer *layout) (evalFn, error) {
	fs := make([]evalFn, len(terms))
	for i, t := range terms {
		var err error
		if fs[i], err = compileExpr(t, cur, outer); err != nil {
			return nil, err
		}
	}
	return func(row value.Row, env *env) (value.Value, error) {
		sawNull := false
		for _, f := range fs {
			v, err := f(row, env)
			if err != nil {
				return value.Null, err
			}
			if v.IsNull() {
				sawNull = true
				continue
			}
			if v.Kind() != value.KindBool {
				return value.Null, fmt.Errorf("exec: boolean operator applied to %s", v.Kind())
			}
			if v.Bool() != isAnd { // false in AND, true in OR decides
				return v, nil
			}
		}
		if sawNull {
			return value.Null, nil
		}
		return value.Bool(isAnd), nil
	}, nil
}

func compareOp(op ir.CmpOp, a, b value.Value) (value.Value, error) {
	if a.IsNull() || b.IsNull() {
		return value.Null, nil
	}
	c, err := compareValues(a, b)
	if err != nil {
		return value.Null, err
	}
	var r bool
	switch op {
	case ir.OpEq:
		r = c == 0
	case ir.OpNe:
		r = c != 0
	case ir.OpLt:
		r = c < 0
	case ir.OpLe:
		r = c <= 0
	case ir.OpGt:
		r = c > 0
	case ir.OpGe:
		r = c >= 0
	default:
		return value.Null, fmt.Errorf("exec: unknown comparison %d", op)
	}
	return value.Bool(r), nil
}

// compareValues compares two non-NULL scalars, promoting int to float and
// parsing strings compared with timestamps.
func compareValues(a, b value.Value) (int, error) {
	if a.Kind() == b.Kind() {
		return value.Compare(a, b), nil
	}
	if a.IsNumeric() && b.IsNumeric() {
		x, y := a.AsFloat(), b.AsFloat()
		switch {
		case x < y:
			return -1, nil
		case x > y:
			return 1, nil
		}
		return 0, nil
	}
	if ta, ok := asTimestamp(a, b); ok {
		return value.Compare(ta, b), nil
	}
	if tb, ok := asTimestamp(b, a); ok {
		return value.Compare(a, tb), nil
	}
	return 0, fmt.Errorf("exec: cannot compare %s with %s", a.Kind(), b.Kind())
}

func asTimestamp(s, other value.Value) (value.Value, bool) {
	if s.Kind() != value.KindString || other.Kind() != value.KindTimestamp {
		return value.Null, false
	}
	return value.ParseTimestamp(s.Str())
}

var errDivZero = errors.New("exec: division by zero")

func arith(op ir.ArithOp, a, b value.Value) (value.Value, error) {
	if a.IsNull() || b.IsNull() {
		return value.Null, nil
	}
	if !a.IsNumeric() || !b.IsNumeric() {
		return value.Null, fmt.Errorf("exec: arithmetic on %s and %s", a.Kind(), b.Kind())
	}
	if a.Kind() == value.KindInt && b.Kind() == value.KindInt {
		x, y := a.Int(), b.Int()
		switch op {
		case ir.OpAdd:
			return value.Int(x + y), nil
		case ir.OpSub:
			return value.Int(x - y), nil
		case ir.OpMul:
			return value.Int(x * y), nil
		case ir.OpDiv, ir.OpMod:
			if y == 0 {
				return value.Null, errDivZero
			}
			if op == ir.OpDiv {
				return value.Int(x / y), nil
			}
			return value.Int(x % y), nil
		}
	}
	x, y := a.AsFloat(), b.AsFloat()
	switch op {
	case ir.OpAdd:
		return value.Float(x + y), nil
	case ir.OpSub:
		return value.Float(x - y), nil
	case ir.OpMul:
		return value.Float(x * y), nil
	case ir.OpDiv:
		if y == 0 {
			return value.Null, errDivZero
		}
		return value.Float(x / y), nil
	case ir.OpMod:
		if y == 0 {
			return value.Null, errDivZero
		}
		return value.Float(math.Mod(x, y)), nil
	}
	return value.Null, fmt.Errorf("exec: unknown arithmetic operator %d", op)
}

// evalAll evaluates a list of expressions.
func evalAll(fs []evalFn, row value.Row, env *env) ([]value.Value, error) {
	out := make([]value.Value, len(fs))
	for i, f := range fs {
		v, err := f(row, env)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func compileAll(es []ir.Expr, cur, outer *layout) ([]evalFn, error) {
	fs := make([]evalFn, len(es))
	for i, e := range es {
		var err error
		if fs[i], err = compileExpr(e, cur, outer); err != nil {
			return nil, err
		}
	}
	return fs, nil
}

func isTrue(v value.Value) bool { return v.Kind() == value.KindBool && v.Bool() }
