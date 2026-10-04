package schema

import (
	"strings"
	"testing"

	"github.com/fpt/go-dquery/value"
)

const shopYAML = `
relations:
  - name: users
    columns:
      - {name: id, type: int}
      - {name: name, type: string}
      - {name: email, type: string}
    primary: {partition: [id]}
    paths:
      - {name: by_email, partition: [email], unique: true}
  - name: orders
    columns:
      - {name: id, type: int}
      - {name: user_id, type: int}
      - {name: amount, type: float}
      - {name: created_at, type: timestamp}
      - {name: note, type: string, nullable: true}
    primary: {partition: [id]}
    paths:
      - {name: by_user, partition: [user_id], sort: [created_at]}
relationships:
  - {name: orders, from: users, from_cols: [id], to: orders, path: by_user, kind: one_to_many}
  - {name: user, from: orders, from_cols: [user_id], to: users, kind: many_to_one}
`

func TestParse(t *testing.T) {
	c, err := Parse([]byte(shopYAML))
	if err != nil {
		t.Fatal(err)
	}
	orders := c.Relation("orders")
	if orders == nil || orders.ID != 2 {
		t.Fatalf("orders relation: %+v", orders)
	}
	byUser := orders.Path("by_user")
	if byUser.ID != 2 || len(byUser.Partition) != 1 || byUser.Sort[0] != orders.Column("created_at").ID {
		t.Fatalf("by_user path: %+v", byUser)
	}
	if !orders.Primary.Primary || !orders.Primary.Unique || orders.Primary.ID != PrimaryPathID {
		t.Fatalf("primary path: %+v", orders.Primary)
	}
	rs := c.Relationship(c.Relation("users"), "orders")
	if rs == nil || rs.Kind != OneToMany || rs.Path != byUser {
		t.Fatalf("relationship: %+v", rs)
	}
}

func TestBuildErrors(t *testing.T) {
	cases := map[string]string{
		"unknown type":     `relations: [{name: a, columns: [{name: id, type: nope}], primary: {partition: [id]}}]`,
		"no pk":            `relations: [{name: a, columns: [{name: id, type: int}]}]`,
		"nullable pk":      `relations: [{name: a, columns: [{name: id, type: int, nullable: true}], primary: {partition: [id]}}]`,
		"unknown path col": `relations: [{name: a, columns: [{name: id, type: int}], primary: {partition: [id]}, paths: [{name: p, partition: [x]}]}]`,
		"dup relation":     `relations: [{name: a, columns: [{name: id, type: int}], primary: {partition: [id]}}, {name: a, columns: [{name: id, type: int}], primary: {partition: [id]}}]`,
		"m2o needs unique": `
relations:
  - {name: a, columns: [{name: id, type: int}, {name: x, type: int}], primary: {partition: [id]}, paths: [{name: by_x, partition: [x]}]}
relationships:
  - {name: r, from: a, from_cols: [id], to: a, path: by_x, kind: many_to_one}`,
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCoerce(t *testing.T) {
	c := MustBuild(CatalogDef{Relations: []RelationDef{{
		Name: "t",
		Columns: []ColumnDef{
			{Name: "id", Type: "int"},
			{Name: "f", Type: "float", Nullable: true},
			{Name: "ts", Type: "timestamp"},
		},
		Primary: PathDef{Partition: []string{"id"}},
	}}})
	rel := c.Relation("t")
	if v, err := rel.Column("f").Coerce(value.Int(3)); err != nil || v.Float() != 3 {
		t.Fatalf("int->float: %v %v", v, err)
	}
	if _, err := rel.Column("id").Coerce(value.Null); err == nil {
		t.Fatal("expected not-null error")
	}
	if _, err := rel.Column("id").Coerce(value.String("x")); err == nil || !strings.Contains(err.Error(), "cannot use") {
		t.Fatalf("expected type error, got %v", err)
	}
	if v, err := rel.Column("ts").Coerce(value.String("2026-10-04T12:00:00Z")); err != nil || v.Kind() != value.KindTimestamp {
		t.Fatalf("string->timestamp: %v %v", v, err)
	}
}
