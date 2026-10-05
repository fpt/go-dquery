package exec_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fpt/go-dquery/exec"
	"github.com/fpt/go-dquery/internal/fixture"
	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/storage/memory"
	"github.com/fpt/go-dquery/value"
)

var ctx = context.Background()

type env struct {
	t     *testing.T
	cat   *schema.Catalog
	ex    *exec.Executor
	users *schema.Relation
	ords  *schema.Relation
	items *schema.Relation
}

func setup(t *testing.T) *env {
	t.Helper()
	cat := fixture.Shop()
	e := &env{t: t, cat: cat, ex: exec.New(memory.NewStore()),
		users: cat.Relation("users"), ords: cat.Relation("orders"), items: cat.Relation("items")}
	e.ex.BatchSize = 2 // exercise multi-batch paths

	e.mustRun(&ir.Insert{Rel: e.users, Rows: [][]ir.Expr{
		{ir.I(1), ir.S("ann"), ir.S("ann@x")},
		{ir.I(2), ir.S("bob"), ir.V(value.Null)},
		{ir.I(3), ir.S("cat"), ir.S("cat@x")},
	}})
	var orders [][]ir.Expr
	for _, o := range []value.Row{
		fixture.Order(10, 1, "open", 5, 1),
		fixture.Order(11, 1, "paid", 7, 2),
		fixture.Order(12, 1, "open", 9, 3),
		fixture.Order(13, 2, "open", 11, 4),
		fixture.Order(14, 9, "open", 1, 5), // dangling user
	} {
		orders = append(orders, lits(o))
	}
	e.mustRun(&ir.Insert{Rel: e.ords, Rows: orders})
	e.mustRun(&ir.Insert{Rel: e.items, Cols: []string{"order_id", "line", "product", "qty"}, Rows: [][]ir.Expr{
		lits(fixture.Item(10, 1, "pen", 2)),
		lits(fixture.Item(10, 2, "ink", 1)),
		lits(fixture.Item(12, 1, "pad", 3)),
	}})
	return e
}

func lits(r value.Row) []ir.Expr {
	out := make([]ir.Expr, len(r))
	for i, v := range r {
		out[i] = ir.V(v)
	}
	return out
}

func (e *env) run(plan ir.Node, params ...value.Value) (*exec.Result, error) {
	return e.ex.Execute(ctx, &ir.Return{Input: plan}, params...)
}

func (e *env) mustRun(plan ir.Node, params ...value.Value) *exec.Result {
	e.t.Helper()
	res, err := e.run(plan, params...)
	if err != nil {
		e.t.Fatalf("execute:\n%s\nerror: %v", ir.Explain(plan), err)
	}
	return res
}

func render(res *exec.Result) string {
	var b strings.Builder
	b.WriteString(strings.Join(res.Columns, " | "))
	b.WriteByte('\n')
	for _, r := range res.Rows {
		parts := make([]string, len(r))
		for i, v := range r {
			parts[i] = v.String()
		}
		b.WriteString(strings.Join(parts, " | "))
		b.WriteByte('\n')
	}
	return b.String()
}

func check(t *testing.T, res *exec.Result, want string) {
	t.Helper()
	want = strings.TrimLeft(want, "\n")
	if got := render(res); got != want {
		t.Fatalf("result mismatch\n--- got\n%s--- want\n%s", got, want)
	}
}

func TestGetLookupProject(t *testing.T) {
	e := setup(t)
	// The latest two orders of user $1, newest first.
	plan := &ir.Project{
		Input: &ir.Lookup{
			Input: &ir.Get{Rel: e.users, Alias: "u", Path: e.users.Primary, Key: ir.Exprs(ir.P(1))},
			Rel:   e.ords, Alias: "o", Path: e.ords.Path("by_user"),
			Key: ir.Exprs(ir.C("u", "id")), Many: true, Reverse: true, Limit: 2,
		},
		Exprs: []ir.NamedExpr{ir.As("name", ir.C("u", "name")), ir.As("id", ir.C("o", "id")), ir.As("amount", ir.C("o", "amount"))},
	}
	check(t, e.mustRun(plan, value.Int(1)), `
name | id | amount
"ann" | 12 | 9
"ann" | 11 | 7
`)

	wantExplain := `Project [u.name, o.id, o.amount]
  Lookup orders o via by_user key=[u.id] many reverse limit=2
    Get users u via primary key=[$1]
`
	if got := ir.Explain(plan); got != wantExplain {
		t.Fatalf("explain mismatch\n--- got\n%s--- want\n%s", got, wantExplain)
	}
}

func TestScanFilterBounds(t *testing.T) {
	e := setup(t)
	plan := &ir.Project{
		Input: &ir.Filter{
			Input: &ir.Scan{Rel: e.ords, Path: e.ords.Path("by_status"), Eq: ir.Exprs(ir.S("open")),
				Lo: &ir.Bound{Expr: ir.S("2026-01-02T00:00:00Z")}}, // exclusive: skips order 10
			Pred: ir.Gt(ir.C("", "amount"), ir.I(2)),
		},
		Exprs: []ir.NamedExpr{ir.As("id", ir.C("orders", "id"))},
	}
	check(t, e.mustRun(plan), `
id
12
13
`)
}

func TestLookupManyToOneOptional(t *testing.T) {
	e := setup(t)
	plan := &ir.Project{
		Input: &ir.Lookup{
			Input: &ir.Scan{Rel: e.ords, Alias: "o", Path: e.ords.Primary},
			Rel:   e.users, Alias: "u", Path: e.users.Primary, Key: ir.Exprs(ir.C("o", "user_id")),
			Optional: true,
		},
		Exprs: []ir.NamedExpr{ir.As("order", ir.C("o", "id")), ir.As("user", ir.C("u", "name"))},
	}
	check(t, e.mustRun(plan), `
order | user
10 | "ann"
11 | "ann"
12 | "ann"
13 | "bob"
14 | NULL
`)
	// Inner variant drops the dangling order.
	plan.Input.(*ir.Lookup).Optional = false
	if n := len(e.mustRun(plan).Rows); n != 4 {
		t.Fatalf("inner lookup: got %d rows, want 4", n)
	}
}

func TestLookupByUniqueSecondary(t *testing.T) {
	e := setup(t)
	plan := &ir.Get{Rel: e.users, Path: e.users.Path("by_email"), Key: ir.Exprs(ir.P(1))}
	check(t, e.mustRun(plan, value.String("cat@x")), `
id | name | email
3 | "cat" | "cat@x"
`)
	if rows := e.mustRun(plan, value.Null).Rows; len(rows) != 0 {
		t.Fatalf("NULL key matched %v", rows)
	}
}

func TestMapNested(t *testing.T) {
	e := setup(t)
	itemsOf := &ir.Project{
		Input: &ir.Scan{Rel: e.items, Path: e.items.Primary, Eq: ir.Exprs(ir.O("o", "id"))},
		Exprs: []ir.NamedExpr{ir.As("product", ir.C("", "product")), ir.As("qty", ir.C("", "qty"))},
	}
	ordersOf := &ir.Project{
		Input: &ir.Map{
			Input:  &ir.Scan{Rel: e.ords, Alias: "o", Path: e.ords.Path("by_user"), Eq: ir.Exprs(ir.O("u", "id")), Limit: 2},
			Fields: []ir.MapField{{Name: "items", Plan: itemsOf}},
		},
		Exprs: []ir.NamedExpr{ir.As("id", ir.C("o", "id")), ir.As("items", ir.C("", "items"))},
	}
	plan := &ir.Project{
		Input: &ir.Map{
			Input:  &ir.GetMany{Rel: e.users, Alias: "u", Path: e.users.Primary, Keys: [][]ir.Expr{{ir.I(1)}, {ir.I(2)}}},
			Fields: []ir.MapField{{Name: "orders", Plan: ordersOf}},
		},
		Exprs: []ir.NamedExpr{ir.As("name", ir.C("u", "name")), ir.As("orders", ir.C("", "orders"))},
	}
	check(t, e.mustRun(plan), `
name | orders
"ann" | [{id: 10, items: [{product: "pen", qty: 2}, {product: "ink", qty: 1}]}, {id: 11, items: []}]
"bob" | [{id: 13, items: []}]
`)
	if !strings.Contains(ir.Explain(plan), "field orders (many)") {
		t.Fatalf("explain lacks map field:\n%s", ir.Explain(plan))
	}
}

func TestUpdateReturningAndIndexes(t *testing.T) {
	e := setup(t)
	upd := &ir.Update{
		Rel:   e.ords,
		Input: &ir.Scan{Rel: e.ords, Path: e.ords.Path("by_user"), Eq: ir.Exprs(ir.P(1))},
		Set: []ir.Assign{
			{Col: "status", Expr: ir.S("paid")},
			{Col: "amount", Expr: ir.Arith{Op: ir.OpMul, L: ir.C("", "amount"), R: ir.I(2)}},
		},
		Returning: []ir.NamedExpr{ir.As("id", ir.C("", "id")), ir.As("amount", ir.C("", "amount"))},
	}
	res := e.mustRun(upd, value.Int(1))
	if res.RowsAffected != 3 {
		t.Fatalf("rows affected %d", res.RowsAffected)
	}
	check(t, res, `
id | amount
10 | 10
11 | 14
12 | 18
`)
	paid := &ir.Project{
		Input: &ir.Scan{Rel: e.ords, Path: e.ords.Path("by_status"), Eq: ir.Exprs(ir.S("paid"))},
		Exprs: []ir.NamedExpr{ir.As("id", ir.C("", "id"))},
	}
	check(t, e.mustRun(paid), "id\n10\n11\n12\n")

	if _, err := e.run(&ir.Update{Rel: e.ords, Input: upd.Input, Set: []ir.Assign{{Col: "id", Expr: ir.I(1)}}}, value.Int(1)); err == nil {
		t.Fatal("expected error updating a primary key column")
	}
}

func TestUpdateThroughLookup(t *testing.T) {
	e := setup(t)
	// UPDATE orders o SET amount = 0 FROM users u WHERE u.email = $1 AND o.user_id = u.id
	upd := &ir.Update{
		Rel: e.ords, Alias: "o",
		Input: &ir.Lookup{
			Input: &ir.Get{Rel: e.users, Alias: "u", Path: e.users.Path("by_email"), Key: ir.Exprs(ir.P(1))},
			Rel:   e.ords, Alias: "o", Path: e.ords.Path("by_user"), Key: ir.Exprs(ir.C("u", "id")), Many: true,
		},
		Set: []ir.Assign{{Col: "amount", Expr: ir.I(0)}},
	}
	if res := e.mustRun(upd, value.String("ann@x")); res.RowsAffected != 3 {
		t.Fatalf("rows affected %d", res.RowsAffected)
	}
	sum := e.mustRun(&ir.Filter{Input: &ir.Scan{Rel: e.ords, Path: e.ords.Primary}, Pred: ir.Eq(ir.C("", "amount"), ir.F(0))})
	if len(sum.Rows) != 3 {
		t.Fatalf("got %d zeroed orders", len(sum.Rows))
	}
}

func TestDeleteReturning(t *testing.T) {
	e := setup(t)
	del := &ir.Delete{
		Rel:       e.users,
		Input:     &ir.Get{Rel: e.users, Path: e.users.Path("by_email"), Key: ir.Exprs(ir.S("ann@x"))},
		Returning: []ir.NamedExpr{ir.As("id", ir.C("", "id"))},
	}
	check(t, e.mustRun(del), "id\n1\n")
	if rows := e.mustRun(del.Input).Rows; len(rows) != 0 {
		t.Fatalf("index still finds deleted row: %v", rows)
	}
	if res := e.mustRun(del); res.RowsAffected != 0 {
		t.Fatalf("second delete affected %d", res.RowsAffected)
	}
}

func TestInsertConflicts(t *testing.T) {
	e := setup(t)
	ins := func(oc *ir.OnConflict, rows ...[]ir.Expr) (*exec.Result, error) {
		return e.run(&ir.Insert{Rel: e.users, Rows: rows, OnConflict: oc,
			Returning: []ir.NamedExpr{ir.As("id", ir.C("", "id")), ir.As("name", ir.C("", "name"))}})
	}
	if _, err := ins(nil, []ir.Expr{ir.I(1), ir.S("dup"), ir.V(value.Null)}); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("plain duplicate: %v", err)
	}
	// Atomic: the valid row is not applied when another row conflicts on a unique path.
	if _, err := ins(nil, []ir.Expr{ir.I(4), ir.S("dan"), ir.V(value.Null)}, []ir.Expr{ir.I(5), ir.S("eve"), ir.S("ann@x")}); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("unique duplicate: %v", err)
	}
	if rows := e.mustRun(&ir.Get{Rel: e.users, Path: e.users.Primary, Key: ir.Exprs(ir.I(4))}).Rows; len(rows) != 0 {
		t.Fatal("partial insert applied")
	}

	res, err := ins(&ir.OnConflict{DoNothing: true},
		[]ir.Expr{ir.I(1), ir.S("dup"), ir.V(value.Null)},
		[]ir.Expr{ir.I(4), ir.S("dan"), ir.V(value.Null)},
		[]ir.Expr{ir.I(4), ir.S("dan2"), ir.V(value.Null)})
	if err != nil {
		t.Fatal(err)
	}
	check(t, res, "id | name\n4 | \"dan\"\n")

	res, err = ins(&ir.OnConflict{Set: []ir.Assign{{Col: "name", Expr: ir.Add(ir.C("excluded", "name"), ir.V(value.Null))}}},
		[]ir.Expr{ir.I(2), ir.S("bobby"), ir.V(value.Null)})
	if err == nil {
		t.Fatal("expected arithmetic on strings to fail")
	}
	res, err = ins(&ir.OnConflict{Set: []ir.Assign{{Col: "name", Expr: ir.C("excluded", "name")}}},
		[]ir.Expr{ir.I(2), ir.S("bobby"), ir.V(value.Null)},
		[]ir.Expr{ir.I(6), ir.S("fay"), ir.V(value.Null)})
	if err != nil {
		t.Fatal(err)
	}
	check(t, res, "id | name\n2 | \"bobby\"\n6 | \"fay\"\n")
}

func TestInsertFromInput(t *testing.T) {
	e := setup(t)
	// Copy order 10's items to order 12 with renumbered lines.
	plan := &ir.Insert{
		Rel: e.items,
		Input: &ir.Project{
			Input: &ir.Scan{Rel: e.items, Path: e.items.Primary, Eq: ir.Exprs(ir.I(10))},
			Exprs: []ir.NamedExpr{
				ir.As("order_id", ir.I(12)),
				ir.As("line", ir.Add(ir.C("", "line"), ir.I(10))),
				ir.As("product", ir.C("", "product")),
				ir.As("qty", ir.C("", "qty")),
			},
		},
	}
	if res := e.mustRun(plan); res.RowsAffected != 2 {
		t.Fatalf("rows affected %d", res.RowsAffected)
	}
	got := e.mustRun(&ir.Project{
		Input: &ir.Scan{Rel: e.items, Path: e.items.Primary, Eq: ir.Exprs(ir.I(12))},
		Exprs: []ir.NamedExpr{ir.As("line", ir.C("", "line"))},
	})
	check(t, got, "line\n1\n11\n12\n")
}

func TestValidationAndResolutionErrors(t *testing.T) {
	e := setup(t)
	cases := map[string]ir.Node{
		"get on non-unique path": &ir.Get{Rel: e.ords, Path: e.ords.Path("by_user"), Key: ir.Exprs(ir.I(1))},
		"foreign path":           &ir.Get{Rel: e.ords, Path: e.users.Path("by_email"), Key: ir.Exprs(ir.I(1))},
		"scan unbound partition": &ir.Scan{Rel: e.ords, Path: e.ords.Path("by_user"), Lo: &ir.Bound{Expr: ir.I(1)}},
		"unknown column":         &ir.Filter{Input: &ir.Scan{Rel: e.ords, Path: e.ords.Primary}, Pred: ir.Eq(ir.C("", "nope"), ir.I(1))},
		"outer outside map":      &ir.Get{Rel: e.users, Path: e.users.Primary, Key: ir.Exprs(ir.O("u", "id"))},
		"ambiguous column": &ir.Filter{
			Input: &ir.Lookup{Input: &ir.Scan{Rel: e.ords, Path: e.ords.Primary}, Rel: e.users, Path: e.users.Primary, Key: ir.Exprs(ir.C("", "user_id"))},
			Pred:  ir.Eq(ir.C("", "id"), ir.I(1)),
		},
		"type mismatch": &ir.Filter{Input: &ir.Scan{Rel: e.ords, Path: e.ords.Primary}, Pred: ir.Eq(ir.C("", "status"), ir.I(1))},
		"missing param": &ir.Get{Rel: e.users, Path: e.users.Primary, Key: ir.Exprs(ir.P(2))},
	}
	for name, plan := range cases {
		if _, err := e.run(plan); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestMapBatchedMatchesUnbatched(t *testing.T) {
	e := setup(t)
	build := func(batched bool) ir.Node {
		return &ir.Project{
			Input: &ir.Map{
				Input: &ir.Scan{Rel: e.ords, Alias: "o", Path: e.ords.Primary},
				Fields: []ir.MapField{
					{Name: "user", One: true, Batched: batched, Plan: &ir.Project{
						Input: &ir.Get{Rel: e.users, Path: e.users.Primary, Key: ir.Exprs(ir.O("o", "user_id"))},
						Exprs: []ir.NamedExpr{ir.As("name", ir.C("", "name"))},
					}},
					{Name: "items", Batched: batched, Plan: &ir.Limit{N: 1, Input: &ir.Scan{
						Rel: e.items, Path: e.items.Primary, Eq: ir.Exprs(ir.O("o", "id")), Reverse: true}}},
				},
			},
			Exprs: []ir.NamedExpr{ir.As("id", ir.C("o", "id")), ir.As("user", ir.C("", "user")), ir.As("items", ir.C("", "items"))},
		}
	}
	want := `
id | user | items
10 | {name: "ann"} | [{order_id: 10, line: 2, product: "ink", qty: 1}]
11 | {name: "ann"} | []
12 | {name: "ann"} | [{order_id: 12, line: 1, product: "pad", qty: 3}]
13 | {name: "bob"} | []
14 | NULL | []
`
	check(t, e.mustRun(build(false)), want)
	batched := build(true)
	if !strings.Contains(ir.Explain(batched), "(one, batched)") {
		t.Fatalf("explain:\n%s", ir.Explain(batched))
	}
	check(t, e.mustRun(batched), want)
}
