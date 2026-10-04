// Package schema defines the catalog: relations, columns, access paths and
// relationships.
package schema

import (
	"fmt"
	"slices"
	"time"

	"github.com/fpt/go-dquery/value"
)

// Type is a column type.
type Type uint8

const (
	TypeBool Type = iota + 1
	TypeInt
	TypeFloat
	TypeString
	TypeBytes
	TypeTimestamp
)

var typeNames = map[Type]string{
	TypeBool:      "bool",
	TypeInt:       "int",
	TypeFloat:     "float",
	TypeString:    "string",
	TypeBytes:     "bytes",
	TypeTimestamp: "timestamp",
}

func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// ParseType parses a type name as used in schema files.
func ParseType(s string) (Type, error) {
	for t, n := range typeNames {
		if n == s {
			return t, nil
		}
	}
	return 0, fmt.Errorf("schema: unknown type %q", s)
}

// ColID is the position of a column within its relation.
type ColID int

type Column struct {
	ID       ColID
	Name     string
	Type     Type
	Nullable bool
}

// Coerce converts v to the column type, or returns an error if it cannot.
// Only lossless conversions are applied (int -> float).
func (c *Column) Coerce(v value.Value) (value.Value, error) {
	if v.IsNull() {
		if !c.Nullable {
			return v, fmt.Errorf("schema: column %q is not nullable", c.Name)
		}
		return v, nil
	}
	want := c.Type.Kind()
	if v.Kind() == want {
		return v, nil
	}
	if want == value.KindFloat && v.Kind() == value.KindInt {
		return value.Float(float64(v.Int())), nil
	}
	if want == value.KindTimestamp && v.Kind() == value.KindString {
		t, err := time.Parse(time.RFC3339Nano, v.Str())
		if err == nil {
			return value.Timestamp(t), nil
		}
	}
	return v, fmt.Errorf("schema: column %q: cannot use %s as %s", c.Name, v.Kind(), c.Type)
}

// Kind returns the value kind that stores this type.
func (t Type) Kind() value.Kind {
	switch t {
	case TypeBool:
		return value.KindBool
	case TypeInt:
		return value.KindInt
	case TypeFloat:
		return value.KindFloat
	case TypeString:
		return value.KindString
	case TypeBytes:
		return value.KindBytes
	case TypeTimestamp:
		return value.KindTimestamp
	}
	return value.KindNull
}

// PathID identifies an access path within a relation. The primary path is
// always PrimaryPathID.
type PathID uint32

const PrimaryPathID PathID = 1

// AccessPath is an ordered way to reach rows of a relation. Partition
// columns must be bound by equality; Sort columns may be bound by an equality
// prefix followed by a range, and define the scan order.
type AccessPath struct {
	ID        PathID
	Name      string
	Partition []ColID
	Sort      []ColID
	Unique    bool
	Primary   bool
}

// KeyCols returns the full key column list: partition followed by sort.
func (p *AccessPath) KeyCols() []ColID {
	out := make([]ColID, 0, len(p.Partition)+len(p.Sort))
	out = append(out, p.Partition...)
	return append(out, p.Sort...)
}

type RelID uint32

type Relation struct {
	ID      RelID
	Name    string
	Columns []*Column
	Primary *AccessPath
	Paths   []*AccessPath // secondary paths

	colByName  map[string]*Column
	pathByName map[string]*AccessPath
}

// Column returns the named column or nil.
func (r *Relation) Column(name string) *Column { return r.colByName[name] }

// Path returns the named access path (including "primary") or nil.
func (r *Relation) Path(name string) *AccessPath { return r.pathByName[name] }

// AllPaths returns the primary path followed by secondary paths.
func (r *Relation) AllPaths() []*AccessPath {
	return append([]*AccessPath{r.Primary}, r.Paths...)
}

// PathByID returns the path with the given ID or nil.
func (r *Relation) PathByID(id PathID) *AccessPath {
	for _, p := range r.AllPaths() {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// KeyOf extracts the values of cols from row.
func KeyOf(row value.Row, cols []ColID) value.Tuple {
	t := make(value.Tuple, len(cols))
	for i, c := range cols {
		t[i] = row[c]
	}
	return t
}

// PK extracts the primary key tuple from a full row.
func (r *Relation) PK(row value.Row) value.Tuple { return KeyOf(row, r.Primary.KeyCols()) }

// IsKeyCol reports whether c is part of the primary key.
func (r *Relation) IsKeyCol(c ColID) bool {
	return slices.Contains(r.Primary.KeyCols(), c)
}

type RelationshipKind uint8

const (
	OneToOne RelationshipKind = iota + 1
	OneToMany
	ManyToOne
)

var relKindNames = map[RelationshipKind]string{
	OneToOne:  "one_to_one",
	OneToMany: "one_to_many",
	ManyToOne: "many_to_one",
}

func (k RelationshipKind) String() string { return relKindNames[k] }

// Relationship is a named traversal from one relation to another through an
// access path of the target. FromCols bind the target path's leading key
// columns.
type Relationship struct {
	Name     string
	From     *Relation
	FromCols []ColID
	To       *Relation
	Path     *AccessPath
	Kind     RelationshipKind
}

// Catalog is an immutable set of relations and relationships.
type Catalog struct {
	relations     []*Relation
	byName        map[string]*Relation
	byID          map[RelID]*Relation
	relationships map[*Relation]map[string]*Relationship
}

func (c *Catalog) Relation(name string) *Relation  { return c.byName[name] }
func (c *Catalog) RelationByID(id RelID) *Relation { return c.byID[id] }
func (c *Catalog) Relations() []*Relation          { return c.relations }
func (c *Catalog) Relationship(from *Relation, name string) *Relationship {
	return c.relationships[from][name]
}

// Relationships returns the relationships starting at from, in no particular
// order.
func (c *Catalog) Relationships(from *Relation) []*Relationship {
	out := make([]*Relationship, 0, len(c.relationships[from]))
	for _, r := range c.relationships[from] {
		out = append(out, r)
	}
	return out
}
