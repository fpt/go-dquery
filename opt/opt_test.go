package opt_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fpt/go-dquery/exec"
	"github.com/fpt/go-dquery/internal/fixture"
	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/opt"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/storage/memory"
	"github.com/fpt/go-dquery/value"
)

var (
	cat   = fixture.Shop()
	users = cat.Relation("users")
	ords  = cat.Relation("orders")
	items = cat.Relation("items")
)

func scan(alias string, rel string) *ir.Scan {
	r := cat.Relation(rel)
	return &ir.Scan{Rel: r, Alias: alias, Path: r.Primary}
}

func where(in ir.Node, terms ...ir.Expr) *ir.Filter {
	return &ir.Filter{Input: in, Pred: conj(terms)}
}

func conj(terms []ir.Expr) ir.Expr {
	if len(terms) == 1 {
		return terms[0]
	}
	return ir.AndOf(terms...)
}

func sortBy(in ir.Node, keys ...ir.SortKey) *ir.Sort { return &ir.Sort{Input: in, Keys: keys} }

func asc(q, n string) ir.SortKey  { return ir.SortKey{Col: ir.C(q, n)} }
func desc(q, n string) ir.SortKey { return ir.SortKey{Col: ir.C(q, n), Desc: true} }

func ordersOf(alias string) *ir.Lookup {
	return &ir.Lookup{Input: nil, Rel: ords, Alias: alias, Path: ords.Path("by_user"), Key: ir.Exprs(ir.C("u", "id")), Many: true}
}

func lookupOrders(in ir.Node) *ir.Lookup {
	l := ordersOf("o")
	l.Input = in
	return l
}

type golden struct {
	name  string
	plan  ir.Node
	want  string
	opts  opt.Options
	err   error
	equiv bool // compare results with the unoptimized plan
}

func goldenCases() []golden {
	return []golden{
		{
			name:  "pk equality becomes Get",
			plan:  where(scan("u", "users"), ir.Eq(ir.C("u", "id"), ir.P(1))),
			want:  "Get users u via primary key=[$1]",
			equiv: true,
		},
		{
			name:  "equality with flipped operands",
			plan:  where(scan("", "users"), ir.Eq(ir.I(2), ir.C("", "id"))),
			want:  "Get users via primary key=[2]",
			equiv: true,
		},
		{
			name: "unique secondary path with residual",
			plan: where(scan("", "users"), ir.Eq(ir.C("", "email"), ir.S("ann@x")), ir.Eq(ir.C("", "name"), ir.S("ann"))),
			want: `
Filter name = "ann"
  Get users via by_email key=["ann@x"]`,
			equiv: true,
		},
		{
			name:  "IN on primary key becomes GetMany",
			plan:  where(scan("", "users"), ir.In{X: ir.C("", "id"), List: ir.Exprs(ir.I(3), ir.I(1), ir.I(9))}),
			want:  "GetMany users via primary keys=[[3], [1], [9]]",
			equiv: true,
		},
		{
			name: "IN on non-key column stays a filter",
			plan: where(scan("", "users"), ir.In{X: ir.C("", "name"), List: ir.Exprs(ir.S("ann"))}),
			err:  opt.ErrFullScan,
		},
		{
			name: "prefix and range with residual",
			plan: where(scan("o", "orders"),
				ir.Eq(ir.C("o", "user_id"), ir.I(1)),
				ir.Ge(ir.C("o", "created_at"), ir.S("2026-01-03T00:00:00Z")),
				ir.Gt(ir.C("o", "amount"), ir.I(6))),
			want: `
Filter o.amount > 6
  Scan orders o via by_user eq=[1] lo>="2026-01-03T00:00:00Z"`,
			equiv: true,
		},
		{
			name: "equal cost and coverage: earlier path wins",
			plan: where(scan("", "orders"),
				ir.Eq(ir.C("", "status"), ir.S("open")),
				ir.Eq(ir.C("", "user_id"), ir.I(1)),
				ir.Lt(ir.C("", "created_at"), ir.S("2026-01-04T00:00:00Z"))),
			want: `
Filter status = "open"
  Scan orders via by_user eq=[1] hi<"2026-01-04T00:00:00Z"`,
			equiv: true,
		},
		{
			name: "range literal of a foreign type is not used as a key",
			plan: where(scan("", "users"), ir.Eq(ir.C("", "id"), ir.F(1.5))),
			err:  opt.ErrFullScan,
		},
		{
			name: "order and limit satisfied by path",
			plan: &ir.Limit{N: 2, Input: sortBy(
				where(scan("", "orders"), ir.Eq(ir.C("", "status"), ir.S("open"))),
				desc("", "created_at"))},
			want:  `Scan orders via by_status eq=["open"] reverse limit=2`,
			equiv: false,
		},
		{
			name: "order on an equality-bound column is free",
			plan: sortBy(where(scan("", "orders"), ir.Eq(ir.C("", "user_id"), ir.I(1))),
				asc("", "user_id"), asc("", "created_at")),
			want: `Scan orders via by_user eq=[1]`,
		},
		{
			name: "limit stays above a residual filter",
			plan: &ir.Limit{N: 1, Input: sortBy(
				where(scan("", "orders"), ir.Eq(ir.C("", "user_id"), ir.I(1)), ir.Gt(ir.C("", "amount"), ir.I(6))),
				desc("", "created_at"))},
			want: `
Limit 1
  Filter amount > 6
    Scan orders via by_user eq=[1] reverse`,
		},
		{
			name: "full scan in pk order with limit",
			plan: &ir.Limit{N: 2, Input: sortBy(scan("", "orders"), desc("", "id"))},
			want: "Scan orders via primary reverse limit=2",
		},
		{
			name: "order that no path provides",
			plan: sortBy(where(scan("", "orders"), ir.Eq(ir.C("", "user_id"), ir.I(1))), asc("", "amount")),
			err:  opt.ErrOrder,
		},
		{
			name: "keys after a full unique key are moot",
			plan: sortBy(where(scan("", "items"), ir.Eq(ir.C("", "order_id"), ir.I(10))), asc("", "order_id"), desc("", "line"), asc("", "qty")),
			want: "Scan items via primary eq=[10] reverse",
		},
		{
			name: "equality prefix with a two-sided range",
			plan: where(scan("", "orders"),
				ir.Eq(ir.C("", "status"), ir.S("open")),
				ir.Eq(ir.C("", "user_id"), ir.I(1)),
				ir.Gt(ir.C("", "created_at"), ir.S("2026-01-02T00:00:00Z")),
				ir.Le(ir.C("", "created_at"), ir.S("2026-01-04T00:00:00Z"))),
			want: `
Filter status = "open"
  Scan orders via by_user eq=[1] lo>"2026-01-02T00:00:00Z" hi<="2026-01-04T00:00:00Z"`,
			equiv: true,
		},
		{
			name: "mixed directions are not satisfiable",
			plan: &ir.Limit{N: 1, Input: sortBy(scan("", "items"), asc("", "order_id"), desc("", "line"))},
			err:  opt.ErrOrder,
		},
		{
			name: "unfiltered full scan is rejected",
			plan: where(scan("", "orders"), ir.Gt(ir.C("", "amount"), ir.I(5))),
			err:  opt.ErrFullScan,
		},
		{
			name: "full scan allowed by option",
			plan: where(scan("", "orders"), ir.Gt(ir.C("", "amount"), ir.I(5))),
			opts: opt.Options{AllowFullScan: true},
			want: `
Filter amount > 5
  Scan orders via primary`,
			equiv: true,
		},
		{
			name: "join predicates pushed below lookup; projection pushdown",
			plan: &ir.Project{
				Input: where(lookupOrders(scan("u", "users")),
					ir.Eq(ir.C("u", "email"), ir.P(1)),
					ir.Eq(ir.C("o", "status"), ir.S("open"))),
				Exprs: []ir.NamedExpr{ir.As("name", ir.C("u", "name")), ir.As("id", ir.C("o", "id"))},
			},
			want: `
Project [u.name, o.id]
  Filter o.status = "open"
    Lookup orders o via by_user key=[u.id] many cols=[id, status]
      Get users u via by_email key=[$1] cols=[id, name]`,
			equiv: true,
		},
		{
			name: "lookup order and limit with a single outer row",
			plan: &ir.Limit{N: 2, Input: sortBy(
				lookupOrders(where(scan("u", "users"), ir.Eq(ir.C("u", "id"), ir.P(1)))),
				desc("o", "created_at"))},
			want: `
Lookup orders o via by_user key=[u.id] many reverse limit=2
  Get users u via primary key=[$1]`,
		},
		{
			name: "lookup order on target needs a single outer row",
			plan: sortBy(lookupOrders(where(scan("u", "users"), ir.In{X: ir.C("u", "id"), List: ir.Exprs(ir.I(1), ir.I(2))})),
				asc("o", "created_at")),
			err: opt.ErrOrder,
		},
		{
			name: "lookup preserves outer order",
			plan: &ir.Limit{N: 3, Input: sortBy(
				&ir.Lookup{Input: scan("o", "orders"), Rel: users, Alias: "u", Path: users.Primary, Key: ir.Exprs(ir.C("o", "user_id")), Optional: true},
				desc("o", "id"))},
			want: `
Lookup users u via primary key=[o.user_id] one optional
  Scan orders o via primary reverse limit=3`,
		},
		{
			name: "map: filter pushdown, batched field with outer key",
			plan: where(&ir.Map{
				Input: scan("u", "users"),
				Fields: []ir.MapField{{Name: "orders", Plan: where(scan("o", "orders"),
					ir.Eq(ir.C("o", "user_id"), ir.O("u", "id")))}},
			}, ir.In{X: ir.C("u", "id"), List: ir.Exprs(ir.I(1), ir.I(2))}),
			want: `
Map
  GetMany users u via primary keys=[[1], [2]]
  field orders (many, batched)
    Scan orders o via by_user eq=[$.u.id]`,
			equiv: true,
		},
		{
			name: "update read side uses access paths",
			plan: &ir.Update{Rel: ords,
				Input: where(scan("", "orders"), ir.Eq(ir.C("", "user_id"), ir.P(1)), ir.Eq(ir.C("", "status"), ir.S("open"))),
				Set:   []ir.Assign{{Col: "status", Expr: ir.S("paid")}}},
			want: `
Update orders set [status = "paid"]
  Filter status = "open"
    Scan orders via by_user eq=[$1]`,
		},
	}
}

func TestGolden(t *testing.T) {
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			before := ir.Explain(tc.plan)
			got, err := opt.Optimize(tc.plan, tc.opts)
			if ir.Explain(tc.plan) != before {
				t.Fatal("Optimize modified its input plan")
			}
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("got error %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("optimize:\n%s\nerror: %v", before, err)
			}
			want := strings.TrimPrefix(tc.want, "\n") + "\n"
			if out := ir.Explain(got); out != want {
				t.Fatalf("plan mismatch\n--- input\n%s--- got\n%s--- want\n%s", before, out, want)
			}
		})
	}
}

// TestEquivalence checks that optimized plans return the same rows as the
// original ones on fixture data.
func TestEquivalence(t *testing.T) {
	ex := exec.New(seed(t))
	params := []value.Value{value.Int(1)}
	for _, tc := range goldenCases() {
		if !tc.equiv {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			got, err := opt.Optimize(tc.plan, opt.Options{AllowFullScan: true})
			if err != nil {
				t.Fatal(err)
			}
			p := params
			if strings.Contains(tc.name, "join") {
				p = []value.Value{value.String("ann@x")}
			}
			a, err := ex.Execute(context.Background(), tc.plan, p...)
			if err != nil {
				t.Fatalf("original: %v", err)
			}
			b, err := ex.Execute(context.Background(), got, p...)
			if err != nil {
				t.Fatalf("optimized: %v", err)
			}
			if ra, rb := rows(a), rows(b); !slices.Equal(ra, rb) {
				t.Fatalf("results differ\n--- original\n%s\n--- optimized\n%s", strings.Join(ra, "\n"), strings.Join(rb, "\n"))
			}
			if len(a.Rows) == 0 {
				t.Fatal("equivalence case returned no rows; fixture does not exercise it")
			}
		})
	}
}

// rows renders a result as sorted lines, ignoring order.
func rows(r *exec.Result) []string {
	out := make([]string, len(r.Rows))
	for i, row := range r.Rows {
		out[i] = row.String()
	}
	slices.Sort(out)
	return out
}

func seed(t *testing.T) storage.Store {
	s := memory.NewStore()
	var ms []storage.Mutation
	for _, u := range []value.Row{fixture.User(1, "ann", "ann@x"), fixture.User(2, "bob", nil), fixture.User(3, "cat", "cat@x")} {
		ms = append(ms, storage.Mutation{Kind: storage.Insert, Rel: users, New: u})
	}
	for _, o := range []value.Row{
		fixture.Order(10, 1, "open", 5, 1),
		fixture.Order(11, 1, "paid", 7, 2),
		fixture.Order(12, 1, "open", 9, 3),
		fixture.Order(13, 2, "open", 11, 4),
		fixture.Order(14, 3, "open", 1, 5),
	} {
		ms = append(ms, storage.Mutation{Kind: storage.Insert, Rel: ords, New: o})
	}
	if err := s.Apply(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOrderedResults(t *testing.T) {
	ex := exec.New(seed(t))
	for _, tc := range []struct {
		name string
		plan ir.Node
		want []int64 // order ids
	}{
		{"open orders, newest two", caseNamed(t, "order and limit satisfied by path").plan, []int64{14, 13}},
		{"user orders newest first", &ir.Project{
			Input: &ir.Limit{N: 2, Input: sortBy(
				lookupOrders(where(scan("u", "users"), ir.Eq(ir.C("u", "id"), ir.I(1)))),
				desc("o", "created_at"))},
			Exprs: []ir.NamedExpr{ir.As("id", ir.C("o", "id"))},
		}, []int64{12, 11}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := opt.Optimize(tc.plan, opt.Options{})
			if err != nil {
				t.Fatal(err)
			}
			res, err := ex.Execute(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, r := range res.Rows {
				ids = append(ids, r[0].Int())
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("got %v, want %v\n%s", ids, tc.want, ir.Explain(plan))
			}
		})
	}
}

func caseNamed(t *testing.T, name string) golden {
	for _, c := range goldenCases() {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no golden case %q", name)
	return golden{}
}
