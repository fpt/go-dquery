// Package value defines the runtime value model: scalar values, rows, tuples
// and nested results, plus an order-preserving key encoding.
package value

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Kind is the dynamic type of a Value. The numeric order of kinds is the
// cross-kind sort order used by Compare and the key encoding.
type Kind uint8

const (
	KindNull Kind = iota
	KindBool
	KindInt
	KindFloat
	KindString
	KindBytes
	KindTimestamp
	KindList   // nested result only; not encodable as a key
	KindRecord // nested result only; not encodable as a key
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindString:
		return "string"
	case KindBytes:
		return "bytes"
	case KindTimestamp:
		return "timestamp"
	case KindList:
		return "list"
	case KindRecord:
		return "record"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Value is an immutable tagged union. The zero Value is NULL.
type Value struct {
	kind Kind
	num  uint64 // bool (0/1), int (int64 bits), float (float64 bits), timestamp (unix nanos)
	str  string // string, bytes
	list []Value
	rec  *Record
}

// Record is a nested result row with named fields.
type Record struct {
	Names  []string
	Values []Value
}

// Get returns the value of the named field and whether it exists.
func (r *Record) Get(name string) (Value, bool) {
	for i, n := range r.Names {
		if n == name {
			return r.Values[i], true
		}
	}
	return Value{}, false
}

var Null = Value{}

func Bool(b bool) Value {
	if b {
		return Value{kind: KindBool, num: 1}
	}
	return Value{kind: KindBool}
}

func Int(i int64) Value     { return Value{kind: KindInt, num: uint64(i)} }
func Float(f float64) Value { return Value{kind: KindFloat, num: math.Float64bits(f)} }
func String(s string) Value { return Value{kind: KindString, str: s} }
func Bytes(b []byte) Value  { return Value{kind: KindBytes, str: string(b)} }

// Timestamp stores t with nanosecond precision in UTC.
func Timestamp(t time.Time) Value {
	return Value{kind: KindTimestamp, num: uint64(t.UnixNano())}
}

func List(vs []Value) Value { return Value{kind: KindList, list: vs} }

func NewRecord(names []string, values []Value) Value {
	return Value{kind: KindRecord, rec: &Record{Names: names, Values: values}}
}

func (v Value) Kind() Kind     { return v.kind }
func (v Value) IsNull() bool   { return v.kind == KindNull }
func (v Value) Bool() bool     { return v.num != 0 }
func (v Value) Int() int64     { return int64(v.num) }
func (v Value) Float() float64 { return math.Float64frombits(v.num) }
func (v Value) Str() string    { return v.str }
func (v Value) Bytes() []byte  { return []byte(v.str) }
func (v Value) Time() time.Time {
	return time.Unix(0, int64(v.num)).UTC()
}
func (v Value) List() []Value   { return v.list }
func (v Value) Record() *Record { return v.rec }
func (v Value) IsNumeric() bool { return v.kind == KindInt || v.kind == KindFloat }
func (v Value) IsScalar() bool  { return v.kind < KindList }
func (v Value) AsFloat() float64 { // numeric kinds only
	if v.kind == KindInt {
		return float64(v.Int())
	}
	return v.Float()
}

// Compare defines a total order: by kind first, then by value. Within
// KindFloat the order matches the key encoding (-NaN < -Inf < ... < -0 < +0 <
// ... < +Inf < NaN). Lists and records compare element-wise.
//
// Compare does not promote numeric kinds; expression evaluation handles
// int/float comparison separately.
func Compare(a, b Value) int {
	if a.kind != b.kind {
		if a.kind < b.kind {
			return -1
		}
		return 1
	}
	switch a.kind {
	case KindNull:
		return 0
	case KindBool, KindTimestamp:
		return cmpOrdered(int64(a.num), int64(b.num))
	case KindInt:
		return cmpOrdered(a.Int(), b.Int())
	case KindFloat:
		return cmpOrdered(floatKey(a.num), floatKey(b.num))
	case KindString, KindBytes:
		return strings.Compare(a.str, b.str)
	case KindList:
		return compareSlices(a.list, b.list)
	case KindRecord:
		return compareSlices(a.rec.Values, b.rec.Values)
	}
	return 0
}

func compareSlices(a, b []Value) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmpOrdered(len(a), len(b))
}

func cmpOrdered[T int | int64 | uint64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Equal reports whether a and b are identical (same kind and value).
func Equal(a, b Value) bool { return Compare(a, b) == 0 }

func (v Value) String() string {
	switch v.kind {
	case KindNull:
		return "NULL"
	case KindBool:
		return strconv.FormatBool(v.Bool())
	case KindInt:
		return strconv.FormatInt(v.Int(), 10)
	case KindFloat:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case KindString:
		return strconv.Quote(v.str)
	case KindBytes:
		return fmt.Sprintf("x'%x'", v.str)
	case KindTimestamp:
		return v.Time().Format(time.RFC3339Nano)
	case KindList:
		var b strings.Builder
		b.WriteByte('[')
		for i, e := range v.list {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(e.String())
		}
		b.WriteByte(']')
		return b.String()
	case KindRecord:
		var b strings.Builder
		b.WriteByte('{')
		for i, n := range v.rec.Names {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(n)
			b.WriteString(": ")
			b.WriteString(v.rec.Values[i].String())
		}
		b.WriteByte('}')
		return b.String()
	}
	return "?"
}

// Row is a positional list of values. Its layout is defined by the plan node
// that produced it.
type Row []Value

func (r Row) String() string {
	parts := make([]string, len(r))
	for i, v := range r {
		parts[i] = v.String()
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// Tuple is an ordered list of scalar values used as a key.
type Tuple []Value

// Equal reports whether two rows are element-wise identical.
func (r Row) Equal(o Row) bool {
	if len(r) != len(o) {
		return false
	}
	for i := range r {
		if !Equal(r[i], o[i]) {
			return false
		}
	}
	return true
}

// HasNull reports whether any element of t is NULL.
func (t Tuple) HasNull() bool {
	for _, v := range t {
		if v.IsNull() {
			return true
		}
	}
	return false
}

// CompareTuples compares element-wise; a strict prefix sorts first.
func CompareTuples(a, b Tuple) int { return compareSlices(a, b) }

// timestampLayouts are the accepted textual timestamp forms; values without
// a zone are interpreted as UTC.
var timestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02",
}

// ParseTimestamp parses s as a timestamp in one of the accepted forms.
func ParseTimestamp(s string) (Value, bool) {
	for _, l := range timestampLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return Timestamp(t), true
		}
	}
	return Null, false
}
