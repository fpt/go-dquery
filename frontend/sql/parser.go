package sql

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/value"
)

// ErrNotSupported reports SQL outside the supported OLTP subset.
var ErrNotSupported = errors.New("sql: not supported")

// Parse parses exactly one statement; a trailing semicolon is optional.
func Parse(src string) (Statement, error) {
	stmts, err := ParseAll(src)
	if err != nil {
		return nil, err
	}
	if len(stmts) != 1 {
		return nil, fmt.Errorf("sql: expected one statement, got %d", len(stmts))
	}
	return stmts[0], nil
}

// ParseAll parses a semicolon-separated script.
func ParseAll(src string) ([]Statement, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	var out []Statement
	p := &parser{toks: toks}
	for {
		for p.acceptOp(";") {
		}
		if p.peek().kind == tokEOF {
			return out, nil
		}
		p.params = paramState{}
		s, err := p.statement()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		if !p.acceptOp(";") && p.peek().kind != tokEOF {
			return nil, p.errorf("expected ; or end of input, got %s", p.peek())
		}
	}
}

// Complete reports whether src ends with a semicolon outside of any quoted
// string or comment, i.e. a REPL can stop reading more lines.
func Complete(src string) bool {
	toks, err := lex(src)
	if err != nil || len(toks) < 2 {
		return false
	}
	last := toks[len(toks)-2]
	return last.kind == tokOp && last.text == ";"
}

type paramState struct {
	positional int  // number of ? seen
	numbered   bool // $n seen
}

type parser struct {
	toks   []token
	i      int
	params paramState
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) peekAt(n int) token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

func (p *parser) errorf(format string, args ...any) error {
	return &SyntaxError{Pos: p.peek().pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) unsupported(what string) error {
	return fmt.Errorf("%w: %s (at %d)", ErrNotSupported, what, p.peek().pos)
}

func (p *parser) isKw(k string) bool { t := p.peek(); return t.kind == tokKeyword && t.text == k }
func (p *parser) isOp(o string) bool { t := p.peek(); return t.kind == tokOp && t.text == o }

func (p *parser) acceptKw(k string) bool {
	if p.isKw(k) {
		p.next()
		return true
	}
	return false
}

func (p *parser) acceptOp(o string) bool {
	if p.isOp(o) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectKw(k string) error {
	if !p.acceptKw(k) {
		return p.errorf("expected %s, got %s", k, p.peek())
	}
	return nil
}

func (p *parser) expectOp(o string) error {
	if !p.acceptOp(o) {
		return p.errorf("expected %q, got %s", o, p.peek())
	}
	return nil
}

func (p *parser) ident() (string, error) {
	t := p.peek()
	if t.kind != tokIdent {
		return "", p.errorf("expected identifier, got %s", t)
	}
	p.next()
	return t.text, nil
}

func (p *parser) statement() (Statement, error) {
	t := p.peek()
	if t.kind != tokKeyword {
		return nil, p.errorf("expected a statement, got %s", t)
	}
	switch t.text {
	case "EXPLAIN":
		p.next()
		s, err := p.statement()
		if err != nil {
			return nil, err
		}
		if _, ok := s.(*Explain); ok {
			return nil, p.errorf("nested EXPLAIN")
		}
		return &Explain{Stmt: s}, nil
	case "SELECT":
		return p.selectStmt()
	case "INSERT":
		return p.insertStmt()
	case "UPDATE":
		return p.updateStmt()
	case "DELETE":
		return p.deleteStmt()
	case "WITH":
		return nil, p.unsupported("WITH")
	}
	return nil, p.errorf("unexpected %s", t)
}

// rejectKw fails on clauses outside the OLTP subset.
func (p *parser) rejectKw(kws ...string) error {
	for _, k := range kws {
		if p.isKw(k) {
			return p.unsupported(k)
		}
	}
	return nil
}

func (p *parser) selectStmt() (*Select, error) {
	p.next() // SELECT
	if err := p.rejectKw("DISTINCT"); err != nil {
		return nil, err
	}
	s := &Select{Limit: -1}
	for {
		item, err := p.selectItem()
		if err != nil {
			return nil, err
		}
		s.Items = append(s.Items, item)
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectKw("FROM"); err != nil {
		return nil, err
	}
	var err error
	if s.From, err = p.tableRef(); err != nil {
		return nil, err
	}
	for {
		if p.isOp(",") {
			return nil, p.unsupported("comma joins; use JOIN ... ON")
		}
		if err := p.rejectKw("RIGHT", "FULL", "CROSS"); err != nil {
			return nil, err
		}
		left := false
		switch {
		case p.acceptKw("JOIN"):
		case p.acceptKw("INNER"):
			if err := p.expectKw("JOIN"); err != nil {
				return nil, err
			}
		case p.acceptKw("LEFT"):
			p.acceptKw("OUTER")
			if err := p.expectKw("JOIN"); err != nil {
				return nil, err
			}
			left = true
		default:
			goto joinsDone
		}
		j := Join{Left: left}
		if j.Table, err = p.tableRef(); err != nil {
			return nil, err
		}
		if err := p.expectKw("ON"); err != nil {
			return nil, err
		}
		if j.On, err = p.expr(); err != nil {
			return nil, err
		}
		s.Joins = append(s.Joins, j)
	}
joinsDone:
	if p.acceptKw("WHERE") {
		if s.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if err := p.rejectKw("GROUP", "HAVING", "UNION", "INTERSECT", "EXCEPT"); err != nil {
		return nil, err
	}
	if p.acceptKw("ORDER") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			item := OrderItem{Expr: e}
			if p.acceptKw("DESC") {
				item.Desc = true
			} else {
				p.acceptKw("ASC")
			}
			s.OrderBy = append(s.OrderBy, item)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKw("LIMIT") {
		t := p.peek()
		if t.kind != tokInt {
			return nil, p.errorf("LIMIT requires an integer literal")
		}
		p.next()
		n, err := strconv.Atoi(t.text)
		if err != nil {
			return nil, &SyntaxError{Pos: t.pos, Msg: "bad LIMIT"}
		}
		s.Limit = n
	}
	if err := p.rejectKw("OFFSET", "UNION", "INTERSECT", "EXCEPT"); err != nil {
		return nil, err
	}
	return s, nil
}

func (p *parser) selectItem() (SelectItem, error) {
	if p.acceptOp("*") {
		return SelectItem{Star: true}, nil
	}
	if p.peek().kind == tokIdent && p.peekAt(1).text == "." && p.peekAt(2).text == "*" {
		q := p.next().text
		p.next()
		p.next()
		return SelectItem{Star: true, Qual: q}, nil
	}
	e, err := p.expr()
	if err != nil {
		return SelectItem{}, err
	}
	item := SelectItem{Expr: e}
	if p.acceptKw("AS") {
		if item.Alias, err = p.ident(); err != nil {
			return SelectItem{}, err
		}
	} else if p.peek().kind == tokIdent {
		item.Alias = p.next().text
	}
	return item, nil
}

func (p *parser) tableRef() (TableRef, error) {
	pos := p.peek().pos
	if p.isOp("(") {
		return TableRef{}, p.unsupported("subqueries")
	}
	name, err := p.ident()
	if err != nil {
		return TableRef{}, err
	}
	t := TableRef{Name: name, Pos: pos}
	if p.acceptKw("AS") {
		if t.Alias, err = p.ident(); err != nil {
			return TableRef{}, err
		}
	} else if p.peek().kind == tokIdent {
		t.Alias = p.next().text
	}
	return t, nil
}

func (p *parser) returning() ([]SelectItem, error) {
	if !p.acceptKw("RETURNING") {
		return nil, nil
	}
	var items []SelectItem
	for {
		item, err := p.selectItem()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if !p.acceptOp(",") {
			return items, nil
		}
	}
}

func (p *parser) identList() ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var out []string
	for {
		id, err := p.ident()
		if err != nil {
			return nil, err
		}
		out = append(out, id)
		if !p.acceptOp(",") {
			break
		}
	}
	return out, p.expectOp(")")
}

func (p *parser) assignments() ([]ir.Assign, error) {
	var out []ir.Assign
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		out = append(out, ir.Assign{Col: col, Expr: e})
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *parser) insertStmt() (*Insert, error) {
	p.next() // INSERT
	if err := p.expectKw("INTO"); err != nil {
		return nil, err
	}
	s := &Insert{}
	var err error
	if s.Table, err = p.tableRef(); err != nil {
		return nil, err
	}
	if p.isOp("(") {
		if s.Cols, err = p.identList(); err != nil {
			return nil, err
		}
	}
	if p.isKw("SELECT") {
		return nil, p.unsupported("INSERT ... SELECT")
	}
	if err := p.expectKw("VALUES"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var row []ir.Expr
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			row = append(row, e)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		s.Rows = append(s.Rows, row)
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("ON") {
		if err := p.expectKw("CONFLICT"); err != nil {
			return nil, err
		}
		c := &Conflict{}
		if p.isOp("(") {
			if c.Target, err = p.identList(); err != nil {
				return nil, err
			}
		}
		if err := p.expectKw("DO"); err != nil {
			return nil, err
		}
		switch {
		case p.acceptKw("NOTHING"):
			c.DoNothing = true
		case p.acceptKw("UPDATE"):
			if err := p.expectKw("SET"); err != nil {
				return nil, err
			}
			if c.Set, err = p.assignments(); err != nil {
				return nil, err
			}
			if p.isKw("WHERE") {
				return nil, p.unsupported("ON CONFLICT ... WHERE")
			}
		default:
			return nil, p.errorf("expected NOTHING or UPDATE, got %s", p.peek())
		}
		s.Conflict = c
	}
	s.Returning, err = p.returning()
	return s, err
}

func (p *parser) updateStmt() (*Update, error) {
	p.next() // UPDATE
	s := &Update{}
	var err error
	if s.Table, err = p.tableRef(); err != nil {
		return nil, err
	}
	if err := p.expectKw("SET"); err != nil {
		return nil, err
	}
	if s.Set, err = p.assignments(); err != nil {
		return nil, err
	}
	if p.isKw("FROM") {
		return nil, p.unsupported("UPDATE ... FROM")
	}
	if p.acceptKw("WHERE") {
		if s.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	s.Returning, err = p.returning()
	return s, err
}

func (p *parser) deleteStmt() (*Delete, error) {
	p.next() // DELETE
	if err := p.expectKw("FROM"); err != nil {
		return nil, err
	}
	s := &Delete{}
	var err error
	if s.Table, err = p.tableRef(); err != nil {
		return nil, err
	}
	if p.acceptKw("WHERE") {
		if s.Where, err = p.expr(); err != nil {
			return nil, err
		}
	}
	s.Returning, err = p.returning()
	return s, err
}

// ---- expressions ----

func (p *parser) expr() (ir.Expr, error) { return p.or() }

func (p *parser) or() (ir.Expr, error) {
	l, err := p.and()
	if err != nil || !p.isKw("OR") {
		return l, err
	}
	terms := []ir.Expr{l}
	for p.acceptKw("OR") {
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		terms = append(terms, r)
	}
	return ir.Or{Terms: terms}, nil
}

func (p *parser) and() (ir.Expr, error) {
	l, err := p.not()
	if err != nil || !p.isKw("AND") {
		return l, err
	}
	terms := []ir.Expr{l}
	for p.acceptKw("AND") {
		r, err := p.not()
		if err != nil {
			return nil, err
		}
		terms = append(terms, r)
	}
	return ir.And{Terms: terms}, nil
}

func (p *parser) not() (ir.Expr, error) {
	if p.acceptKw("NOT") {
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		return ir.Not{X: x}, nil
	}
	return p.predicate()
}

var cmpOps = map[string]ir.CmpOp{"=": ir.OpEq, "<>": ir.OpNe, "!=": ir.OpNe, "<": ir.OpLt, "<=": ir.OpLe, ">": ir.OpGt, ">=": ir.OpGe}

func (p *parser) predicate() (ir.Expr, error) {
	l, err := p.additive()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind == tokOp {
		if op, ok := cmpOps[t.text]; ok {
			p.next()
			r, err := p.additive()
			if err != nil {
				return nil, err
			}
			return ir.Cmp{Op: op, L: l, R: r}, nil
		}
	}
	if p.acceptKw("IS") {
		neg := p.acceptKw("NOT")
		if err := p.expectKw("NULL"); err != nil {
			return nil, err
		}
		return ir.IsNull{X: l, Negate: neg}, nil
	}
	neg := false
	if p.isKw("NOT") && (p.peekAt(1).text == "IN" || p.peekAt(1).text == "BETWEEN") {
		p.next()
		neg = true
	}
	var out ir.Expr
	switch {
	case p.acceptKw("IN"):
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		if p.isKw("SELECT") {
			return nil, p.unsupported("subqueries")
		}
		var list []ir.Expr
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			list = append(list, e)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		out = ir.In{X: l, List: list}
	case p.acceptKw("BETWEEN"):
		lo, err := p.additive()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("AND"); err != nil {
			return nil, err
		}
		hi, err := p.additive()
		if err != nil {
			return nil, err
		}
		out = ir.And{Terms: []ir.Expr{ir.Ge(l, lo), ir.Le(l, hi)}}
	default:
		return l, nil
	}
	if neg {
		return ir.Not{X: out}, nil
	}
	return out, nil
}

func (p *parser) additive() (ir.Expr, error) {
	l, err := p.multiplicative()
	if err != nil {
		return nil, err
	}
	for {
		var op ir.ArithOp
		switch {
		case p.acceptOp("+"):
			op = ir.OpAdd
		case p.acceptOp("-"):
			op = ir.OpSub
		default:
			return l, nil
		}
		r, err := p.multiplicative()
		if err != nil {
			return nil, err
		}
		l = ir.Arith{Op: op, L: l, R: r}
	}
}

func (p *parser) multiplicative() (ir.Expr, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		var op ir.ArithOp
		switch {
		case p.acceptOp("*"):
			op = ir.OpMul
		case p.acceptOp("/"):
			op = ir.OpDiv
		case p.acceptOp("%"):
			op = ir.OpMod
		default:
			return l, nil
		}
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		l = ir.Arith{Op: op, L: l, R: r}
	}
}

func (p *parser) unary() (ir.Expr, error) {
	switch {
	case p.acceptOp("-"):
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		if lit, ok := x.(ir.Lit); ok {
			switch lit.V.Kind() {
			case value.KindInt:
				return ir.I(-lit.V.Int()), nil
			case value.KindFloat:
				return ir.F(-lit.V.Float()), nil
			}
		}
		return ir.Arith{Op: ir.OpSub, L: ir.I(0), R: x}, nil
	case p.acceptOp("+"):
		return p.unary()
	}
	return p.primary()
}

func (p *parser) primary() (ir.Expr, error) {
	t := p.peek()
	switch t.kind {
	case tokInt:
		p.next()
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return nil, &SyntaxError{Pos: t.pos, Msg: "integer out of range"}
		}
		return ir.I(n), nil
	case tokFloat:
		p.next()
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, &SyntaxError{Pos: t.pos, Msg: "bad number"}
		}
		return ir.F(f), nil
	case tokString:
		p.next()
		return ir.S(t.text), nil
	case tokParam:
		p.next()
		return p.param(t)
	case tokKeyword:
		switch t.text {
		case "NULL":
			p.next()
			return ir.V(value.Null), nil
		case "TRUE", "FALSE":
			p.next()
			return ir.V(value.Bool(t.text == "TRUE")), nil
		case "CASE", "EXISTS":
			return nil, p.unsupported(t.text)
		}
	case tokIdent:
		p.next()
		if p.isOp("(") {
			return nil, fmt.Errorf("%w: function %s() (at %d)", ErrNotSupported, t.text, t.pos)
		}
		if p.acceptOp(".") {
			name, err := p.ident()
			if err != nil {
				return nil, err
			}
			return ir.C(t.text, name), nil
		}
		return ir.C("", t.text), nil
	case tokOp:
		if t.text == "(" {
			p.next()
			if p.isKw("SELECT") {
				return nil, p.unsupported("subqueries")
			}
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			return e, p.expectOp(")")
		}
	}
	return nil, p.errorf("unexpected %s", t)
}

func (p *parser) param(t token) (ir.Expr, error) {
	if t.text == "?" {
		if p.params.numbered {
			return nil, &SyntaxError{Pos: t.pos, Msg: "cannot mix ? and $n parameters"}
		}
		p.params.positional++
		return ir.P(p.params.positional), nil
	}
	if p.params.positional > 0 {
		return nil, &SyntaxError{Pos: t.pos, Msg: "cannot mix ? and $n parameters"}
	}
	n, err := strconv.Atoi(t.text[1:])
	if err != nil || n < 1 {
		return nil, &SyntaxError{Pos: t.pos, Msg: "parameters are numbered from $1"}
	}
	p.params.numbered = true
	return ir.P(n), nil
}
