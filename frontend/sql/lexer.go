// Package sql compiles an OLTP subset of SQL into IR plans.
package sql

import (
	"fmt"
	"strings"
	"unicode"
)

type tokKind uint8

const (
	tokEOF tokKind = iota
	tokIdent
	tokKeyword
	tokInt
	tokFloat
	tokString
	tokParam // $n or ?
	tokOp    // punctuation and operators
)

type token struct {
	kind tokKind
	text string // keywords upper-cased; identifiers as written (unquoted)
	pos  int    // byte offset
}

func (t token) String() string {
	switch t.kind {
	case tokEOF:
		return "end of input"
	case tokString:
		return "'" + t.text + "'"
	}
	return fmt.Sprintf("%q", t.text)
}

var keywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`
		SELECT FROM WHERE AND OR NOT IN IS NULL TRUE FALSE AS JOIN INNER LEFT OUTER ON
		ORDER BY ASC DESC LIMIT OFFSET INSERT INTO VALUES UPDATE SET DELETE RETURNING
		CONFLICT DO NOTHING BETWEEN EXPLAIN
		GROUP HAVING DISTINCT UNION INTERSECT EXCEPT RIGHT FULL CROSS WITH CASE EXISTS`) {
		keywords[k] = true
	}
}

// SyntaxError reports a lexing or parsing error with its byte position.
type SyntaxError struct {
	Pos int
	Msg string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("sql: syntax error at %d: %s", e.Pos, e.Msg) }

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && strings.HasPrefix(src[i:], "--"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			word := src[i:j]
			if up := strings.ToUpper(word); keywords[up] {
				toks = append(toks, token{kind: tokKeyword, text: up, pos: i})
			} else {
				toks = append(toks, token{kind: tokIdent, text: strings.ToLower(word), pos: i})
			}
			i = j
		case c == '"':
			s, n, err := readQuoted(src[i:], '"')
			if err != nil {
				return nil, &SyntaxError{Pos: i, Msg: err.Error()}
			}
			toks = append(toks, token{kind: tokIdent, text: s, pos: i})
			i += n
		case c == '\'':
			s, n, err := readQuoted(src[i:], '\'')
			if err != nil {
				return nil, &SyntaxError{Pos: i, Msg: err.Error()}
			}
			toks = append(toks, token{kind: tokString, text: s, pos: i})
			i += n
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9':
			j, float := i, false
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				float = float || src[j] == '.'
				j++
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				float = true
				j++
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			kind := tokInt
			if float {
				kind = tokFloat
			}
			toks = append(toks, token{kind: kind, text: src[i:j], pos: i})
			i = j
		case c == '$':
			j := i + 1
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j == i+1 {
				return nil, &SyntaxError{Pos: i, Msg: "expected parameter number after $"}
			}
			toks = append(toks, token{kind: tokParam, text: src[i:j], pos: i})
			i = j
		case c == '?':
			toks = append(toks, token{kind: tokParam, text: "?", pos: i})
			i++
		default:
			op := ""
			for _, o := range []string{"<=", ">=", "<>", "!=", "=", "<", ">", "(", ")", ",", ".", ";", "*", "+", "-", "/", "%"} {
				if strings.HasPrefix(src[i:], o) {
					op = o
					break
				}
			}
			if op == "" {
				return nil, &SyntaxError{Pos: i, Msg: fmt.Sprintf("unexpected character %q", c)}
			}
			toks = append(toks, token{kind: tokOp, text: op, pos: i})
			i += len(op)
		}
	}
	return append(toks, token{kind: tokEOF, pos: len(src)}), nil
}

// readQuoted reads a quoted string starting at s[0]; a doubled quote is an
// escaped quote. It returns the content and the number of bytes consumed.
func readQuoted(s string, q byte) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != q {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == q {
			b.WriteByte(q)
			i++
			continue
		}
		return b.String(), i + 1, nil
	}
	return "", 0, fmt.Errorf("unterminated %c-quoted string", q)
}

func isIdentStart(c byte) bool {
	return c == '_' || unicode.IsLetter(rune(c))
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}
