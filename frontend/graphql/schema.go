// Package graphql compiles GraphQL operations directly into IR plans, without
// going through SQL. The GraphQL schema is derived from the catalog using
// Hasura-style conventions:
//
//	type Query {
//	  users(where: Users_bool_exp, order_by: [Users_order_by!], limit: Int): [Users!]!
//	  users_by_pk(id: Int!): Users
//	  users_by_email(email: String!): Users       # one per unique secondary path
//	}
//	type Mutation {
//	  insert_users(objects: [Users_insert_input!]!): Users_mutation_response
//	  update_users(where: Users_bool_exp!, _set: Users_set_input!): Users_mutation_response
//	  delete_users(where: Users_bool_exp!): Users_mutation_response
//	}
//
// Relationships become fields: one-to-many as a filtered, ordered, limited
// list; many-to-one and one-to-one as a nullable object.
package graphql

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/fpt/go-dquery/schema"
)

// Schema is the GraphQL view of a catalog.
type Schema struct {
	cat   *schema.Catalog
	ast   *ast.Schema
	sdl   string
	types map[string]*schema.Relation // GraphQL type name -> relation
	roots map[string]rootDef          // Query and Mutation field name -> definition
}

type rootKind uint8

const (
	rootList rootKind = iota + 1
	rootByKey
	rootInsert
	rootUpdate
	rootDelete
)

type rootDef struct {
	kind rootKind
	rel  *schema.Relation
	path *schema.AccessPath // rootByKey only
}

// NewSchema derives a GraphQL schema from cat.
func NewSchema(cat *schema.Catalog) (*Schema, error) {
	s := &Schema{cat: cat, types: map[string]*schema.Relation{}, roots: map[string]rootDef{}}
	for _, rel := range cat.Relations() {
		name := TypeName(rel)
		if other := s.types[name]; other != nil {
			return nil, fmt.Errorf("graphql: relations %s and %s map to the same type %s", other.Name, rel.Name, name)
		}
		s.types[name] = rel
	}
	s.sdl = s.buildSDL()
	var err error
	if s.ast, err = gqlparser.LoadSchema(&ast.Source{Name: "catalog.graphql", Input: s.sdl}); err != nil {
		return nil, fmt.Errorf("graphql: generated schema is invalid: %w", err)
	}
	return s, nil
}

// SDL returns the schema in GraphQL schema definition language.
func (s *Schema) SDL() string { return s.sdl }

// TypeName returns the GraphQL object type name of a relation: its name in
// PascalCase ("order_items" -> "OrderItems").
func TypeName(rel *schema.Relation) string {
	var b strings.Builder
	upper := true
	for _, r := range rel.Name {
		if r == '_' {
			upper = true
			continue
		}
		if upper {
			r = unicode.ToUpper(r)
			upper = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func scalarName(t schema.Type) string {
	switch t {
	case schema.TypeBool:
		return "Boolean"
	case schema.TypeInt:
		return "Int"
	case schema.TypeFloat:
		return "Float"
	case schema.TypeString:
		return "String"
	case schema.TypeBytes:
		return "Bytes"
	case schema.TypeTimestamp:
		return "Timestamp"
	}
	return "String"
}

func colType(c *schema.Column) string {
	if c.Nullable {
		return scalarName(c.Type)
	}
	return scalarName(c.Type) + "!"
}

// relationships returns the relationships of rel sorted by name.
func (s *Schema) relationships(rel *schema.Relation) []*schema.Relationship {
	rs := s.cat.Relationships(rel)
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	return rs
}

func listArgs(t string) string {
	return fmt.Sprintf("(where: %s_bool_exp, order_by: [%s_order_by!], limit: Int)", t, t)
}

func (s *Schema) buildSDL() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("\"Timestamp in RFC 3339 format.\"\nscalar Timestamp\n")
	w("\"Base64-encoded bytes.\"\nscalar Bytes\n\n")
	w("enum order_by {\n  asc\n  desc\n}\n\n")
	for _, sc := range []string{"Boolean", "Int", "Float", "String", "Bytes", "Timestamp"} {
		w("input %s_comparison_exp {\n", sc)
		for _, op := range []string{"_eq", "_neq", "_gt", "_gte", "_lt", "_lte"} {
			w("  %s: %s\n", op, sc)
		}
		w("  _in: [%s!]\n  _is_null: Boolean\n}\n\n", sc)
	}

	var query, mutation strings.Builder
	for _, rel := range s.cat.Relations() {
		t := TypeName(rel)
		w("type %s {\n", t)
		for _, c := range rel.Columns {
			w("  %s: %s\n", c.Name, colType(c))
		}
		for _, r := range s.relationships(rel) {
			to := TypeName(r.To)
			if r.Kind == schema.OneToMany {
				w("  %s%s: [%s!]!\n", r.Name, listArgs(to), to)
			} else {
				w("  %s: %s\n", r.Name, to)
			}
		}
		w("}\n\n")

		w("input %s_bool_exp {\n  _and: [%s_bool_exp!]\n  _or: [%s_bool_exp!]\n  _not: %s_bool_exp\n", t, t, t, t)
		for _, c := range rel.Columns {
			w("  %s: %s_comparison_exp\n", c.Name, scalarName(c.Type))
		}
		w("}\n\n")

		w("input %s_order_by {\n", t)
		for _, c := range rel.Columns {
			w("  %s: order_by\n", c.Name)
		}
		w("}\n\n")

		w("input %s_insert_input {\n", t)
		for _, c := range rel.Columns {
			w("  %s: %s\n", c.Name, colType(c))
		}
		w("}\n\n")

		w("input %s_set_input {\n", t)
		for _, c := range rel.Columns {
			w("  %s: %s\n", c.Name, scalarName(c.Type))
		}
		w("}\n\n")

		w("type %s_mutation_response {\n  affected_rows: Int!\n  returning: [%s!]!\n}\n\n", t, t)

		fmt.Fprintf(&query, "  %s%s: [%s!]!\n", rel.Name, listArgs(t), t)
		s.roots[rel.Name] = rootDef{kind: rootList, rel: rel}
		for _, p := range rel.AllPaths() {
			if !p.Unique {
				continue
			}
			var args []string
			for _, id := range p.KeyCols() {
				c := rel.Columns[id]
				args = append(args, c.Name+": "+scalarName(c.Type)+"!")
			}
			suffix := "by_pk"
			if !p.Primary {
				suffix = p.Name
				if !strings.HasPrefix(suffix, "by_") {
					suffix = "by_" + suffix
				}
			}
			fmt.Fprintf(&query, "  %s_%s(%s): %s\n", rel.Name, suffix, strings.Join(args, ", "), t)
			s.roots[rel.Name+"_"+suffix] = rootDef{kind: rootByKey, rel: rel, path: p}
		}
		fmt.Fprintf(&mutation, "  insert_%s(objects: [%s_insert_input!]!): %s_mutation_response\n", rel.Name, t, t)
		fmt.Fprintf(&mutation, "  update_%s(where: %s_bool_exp!, _set: %s_set_input!): %s_mutation_response\n", rel.Name, t, t, t)
		fmt.Fprintf(&mutation, "  delete_%s(where: %s_bool_exp!): %s_mutation_response\n", rel.Name, t, t)
		s.roots["insert_"+rel.Name] = rootDef{kind: rootInsert, rel: rel}
		s.roots["update_"+rel.Name] = rootDef{kind: rootUpdate, rel: rel}
		s.roots["delete_"+rel.Name] = rootDef{kind: rootDelete, rel: rel}
	}
	w("type Query {\n%s}\n\n", query.String())
	w("type Mutation {\n%s}\n", mutation.String())
	return b.String()
}
