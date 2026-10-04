// Package storagetest is a conformance suite that every storage.Store
// implementation must pass.
package storagetest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/fpt/go-dquery/internal/fixture"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/value"
)

// Factory returns a new, empty store. It is called once per subtest.
type Factory func(t *testing.T) storage.Store

// Run runs the whole suite.
func Run(t *testing.T, newStore Factory) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s storage.Store, c *schema.Catalog)
	}{
		{"InsertGet", testInsertGet},
		{"DuplicateKeys", testDuplicateKeys},
		{"UpdateMaintainsIndexes", testUpdate},
		{"DeleteMaintainsIndexes", testDelete},
		{"OptimisticConflict", testConflict},
		{"BatchAtomicity", testAtomicity},
		{"BatchSeesOwnWrites", testOwnWrites},
		{"ScanRanges", testScanRanges},
		{"RandomizedIndexConsistency", testRandomized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, newStore(t), fixture.Shop())
		})
	}
}

var ctx = context.Background()

func ins(rel *schema.Relation, row value.Row) storage.Mutation {
	return storage.Mutation{Kind: storage.Insert, Rel: rel, New: row}
}

func mustApply(t *testing.T, s storage.Store, ms ...storage.Mutation) {
	t.Helper()
	if err := s.Apply(ctx, ms); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func scanAll(t *testing.T, s storage.Store, req storage.ScanRequest) []value.Row {
	t.Helper()
	it, err := s.Scan(ctx, req)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer it.Close()
	var out []value.Row
	for it.Next() {
		out = append(out, it.Row())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

func col(rows []value.Row, i int) []string {
	out := make([]string, len(rows))
	for j, r := range rows {
		out[j] = r[i].String()
	}
	return out
}

func expect(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func get(t *testing.T, s storage.Store, rel *schema.Relation, path string, key ...value.Value) value.Row {
	t.Helper()
	row, ok, err := s.Get(ctx, rel, rel.Path(path), key, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		return nil
	}
	return row
}

func testInsertGet(t *testing.T, s storage.Store, c *schema.Catalog) {
	users := c.Relation("users")
	mustApply(t, s,
		ins(users, fixture.User(1, "ann", "ann@x")),
		ins(users, fixture.User(2, "bob", nil)),
	)
	if row := get(t, s, users, "primary", value.Int(1)); row == nil || row[1].Str() != "ann" {
		t.Fatalf("get by pk: %v", row)
	}
	if row := get(t, s, users, "by_email", value.String("ann@x")); row == nil || row[0].Int() != 1 {
		t.Fatalf("get by unique path: %v", row)
	}
	if row := get(t, s, users, "primary", value.Int(3)); row != nil {
		t.Fatalf("expected missing, got %v", row)
	}
	if row := get(t, s, users, "by_email", value.Null); row != nil {
		t.Fatalf("NULL key must not match, got %v", row)
	}
	rows, err := s.GetMany(ctx, users, users.Primary, []value.Tuple{{value.Int(2)}, {value.Int(9)}, {value.Int(1)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0][1].Str() != "bob" || rows[1] != nil || rows[2][1].Str() != "ann" {
		t.Fatalf("GetMany: %v", rows)
	}
	if err := s.Apply(ctx, []storage.Mutation{ins(users, value.Row{value.Int(5), value.Null, value.Null})}); err == nil {
		t.Fatal("expected NULL in non-nullable column to fail")
	}
}

func testDuplicateKeys(t *testing.T, s storage.Store, c *schema.Catalog) {
	users := c.Relation("users")
	mustApply(t, s, ins(users, fixture.User(1, "ann", "a@x")), ins(users, fixture.User(2, "bob", nil)))
	for name, row := range map[string]value.Row{
		"pk":     fixture.User(1, "dup", nil),
		"unique": fixture.User(3, "carl", "a@x"),
	} {
		if err := s.Apply(ctx, []storage.Mutation{ins(users, row)}); !errors.Is(err, storage.ErrDuplicateKey) {
			t.Errorf("%s: expected ErrDuplicateKey, got %v", name, err)
		}
	}
	// NULLs never conflict on unique paths.
	mustApply(t, s, ins(users, fixture.User(4, "dan", nil)))
}

func testUpdate(t *testing.T, s storage.Store, c *schema.Catalog) {
	orders := c.Relation("orders")
	o := fixture.Order(10, 1, "open", 5, 1)
	mustApply(t, s, ins(orders, o), ins(orders, fixture.Order(11, 1, "open", 7, 2)))

	n := slices.Clone(o)
	n[1], n[2] = value.Int(2), value.String("paid")
	mustApply(t, s, storage.Mutation{Kind: storage.Update, Rel: orders, Old: o, New: n})

	byUser := func(uid int64) []string {
		return col(scanAll(t, s, storage.ScanRequest{Rel: orders, Path: orders.Path("by_user"), Eq: value.Tuple{value.Int(uid)}}), 0)
	}
	expect(t, "user 1 orders", byUser(1), []string{"11"})
	expect(t, "user 2 orders", byUser(2), []string{"10"})
	paid := scanAll(t, s, storage.ScanRequest{Rel: orders, Path: orders.Path("by_status"), Eq: value.Tuple{value.String("paid")}})
	expect(t, "paid orders", col(paid, 0), []string{"10"})

	pkChange := slices.Clone(n)
	pkChange[0] = value.Int(99)
	if err := s.Apply(ctx, []storage.Mutation{{Kind: storage.Update, Rel: orders, Old: n, New: pkChange}}); err == nil {
		t.Fatal("expected primary key update to fail")
	}
}

func testDelete(t *testing.T, s storage.Store, c *schema.Catalog) {
	users := c.Relation("users")
	u := fixture.User(1, "ann", "a@x")
	mustApply(t, s, ins(users, u))
	mustApply(t, s, storage.Mutation{Kind: storage.Delete, Rel: users, Old: u})
	if row := get(t, s, users, "by_email", value.String("a@x")); row != nil {
		t.Fatalf("index entry survived delete: %v", row)
	}
	// The unique key is free again.
	mustApply(t, s, ins(users, fixture.User(2, "bob", "a@x")))
}

func testConflict(t *testing.T, s storage.Store, c *schema.Catalog) {
	users := c.Relation("users")
	u := fixture.User(1, "ann", nil)
	mustApply(t, s, ins(users, u))
	stale := fixture.User(1, "old", nil)
	upd := fixture.User(1, "new", nil)
	if err := s.Apply(ctx, []storage.Mutation{{Kind: storage.Update, Rel: users, Old: stale, New: upd}}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("update with stale image: got %v", err)
	}
	if err := s.Apply(ctx, []storage.Mutation{{Kind: storage.Delete, Rel: users, Old: fixture.User(7, "x", nil)}}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("delete of missing row: got %v", err)
	}
}

func testAtomicity(t *testing.T, s storage.Store, c *schema.Catalog) {
	users := c.Relation("users")
	mustApply(t, s, ins(users, fixture.User(1, "ann", "a@x")))
	err := s.Apply(ctx, []storage.Mutation{
		ins(users, fixture.User(2, "bob", "b@x")),
		ins(users, fixture.User(3, "carl", "a@x")), // unique violation
	})
	if !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}
	if row := get(t, s, users, "primary", value.Int(2)); row != nil {
		t.Fatalf("partial batch applied: %v", row)
	}
	if row := get(t, s, users, "by_email", value.String("b@x")); row != nil {
		t.Fatalf("partial index write applied: %v", row)
	}
}

func testOwnWrites(t *testing.T, s storage.Store, c *schema.Catalog) {
	users := c.Relation("users")
	if err := s.Apply(ctx, []storage.Mutation{
		ins(users, fixture.User(1, "ann", "a@x")),
		ins(users, fixture.User(1, "ann2", nil)),
	}); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate pk within batch: got %v", err)
	}
	if err := s.Apply(ctx, []storage.Mutation{
		ins(users, fixture.User(1, "ann", "a@x")),
		ins(users, fixture.User(2, "bob", "a@x")),
	}); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate unique key within batch: got %v", err)
	}
	// Insert, update and free a unique key within one batch.
	u := fixture.User(1, "ann", "a@x")
	u2 := fixture.User(1, "ann", "z@x")
	mustApply(t, s,
		ins(users, u),
		storage.Mutation{Kind: storage.Update, Rel: users, Old: u, New: u2},
		ins(users, fixture.User(2, "bob", "a@x")),
	)
	if row := get(t, s, users, "by_email", value.String("a@x")); row == nil || row[0].Int() != 2 {
		t.Fatalf("by_email a@x: %v", row)
	}
}

func testScanRanges(t *testing.T, s storage.Store, c *schema.Catalog) {
	orders := c.Relation("orders")
	var ms []storage.Mutation
	for i := range 10 {
		uid := int64(1 + i%2)
		ms = append(ms, ins(orders, fixture.Order(int64(100+i), uid, "open", float64(i), i)))
	}
	mustApply(t, s, ms...)
	byUser := orders.Path("by_user")
	user1 := value.Tuple{value.Int(1)}
	ids := func(req storage.ScanRequest) []string { return col(scanAll(t, s, req), 0) }

	expect(t, "prefix", ids(storage.ScanRequest{Rel: orders, Path: byUser, Eq: user1}),
		[]string{"100", "102", "104", "106", "108"})
	expect(t, "reverse+limit", ids(storage.ScanRequest{Rel: orders, Path: byUser, Eq: user1, Reverse: true, Limit: 2}),
		[]string{"108", "106"})
	expect(t, "lo inclusive", ids(storage.ScanRequest{Rel: orders, Path: byUser, Eq: user1, Lo: &storage.Bound{Value: fixture.At(4), Inclusive: true}}),
		[]string{"104", "106", "108"})
	expect(t, "lo exclusive", ids(storage.ScanRequest{Rel: orders, Path: byUser, Eq: user1, Lo: &storage.Bound{Value: fixture.At(4)}}),
		[]string{"106", "108"})
	expect(t, "hi inclusive", ids(storage.ScanRequest{Rel: orders, Path: byUser, Eq: user1, Hi: &storage.Bound{Value: fixture.At(4), Inclusive: true}}),
		[]string{"100", "102", "104"})
	expect(t, "lo..hi exclusive reverse", ids(storage.ScanRequest{Rel: orders, Path: byUser, Eq: user1,
		Lo: &storage.Bound{Value: fixture.At(0)}, Hi: &storage.Bound{Value: fixture.At(8)}, Reverse: true}),
		[]string{"106", "104", "102"})
	expect(t, "empty range", ids(storage.ScanRequest{Rel: orders, Path: byUser, Eq: user1,
		Lo: &storage.Bound{Value: fixture.At(6)}, Hi: &storage.Bound{Value: fixture.At(6)}}), nil)
	expect(t, "full primary", ids(storage.ScanRequest{Rel: orders, Path: orders.Primary, Limit: 3}),
		[]string{"100", "101", "102"})
	// A range on a partition column is not a valid access.
	if _, err := s.Scan(ctx, storage.ScanRequest{Rel: orders, Path: orders.Primary,
		Lo: &storage.Bound{Value: value.Int(107), Inclusive: true}}); err == nil {
		t.Fatal("expected error for range on partition column")
	}
	items := c.Relation("items")
	var its []storage.Mutation
	for line := range 5 {
		its = append(its, ins(items, fixture.Item(100, int64(line), "p", 1)))
	}
	mustApply(t, s, its...)
	expect(t, "primary sort range", col(scanAll(t, s, storage.ScanRequest{Rel: items, Path: items.Primary,
		Eq: value.Tuple{value.Int(100)}, Lo: &storage.Bound{Value: value.Int(1)}, Hi: &storage.Bound{Value: value.Int(3), Inclusive: true}}), 1),
		[]string{"2", "3"})
}

// testRandomized applies random mutations and checks every access path
// against a model after each step.
func testRandomized(t *testing.T, s storage.Store, c *schema.Catalog) {
	users := c.Relation("users")
	model := map[int64]value.Row{}
	r := rand.New(rand.NewPCG(7, 8))
	emails := []any{nil, "a", "b", "c", "d", "e"}
	randUser := func(id int64) value.Row {
		return fixture.User(id, fmt.Sprintf("n%d", r.IntN(5)), emails[r.IntN(len(emails))])
	}
	emailTaken := func(row value.Row, exceptID int64) bool {
		if row[2].IsNull() {
			return false
		}
		for id, m := range model {
			if id != exceptID && value.Equal(m[2], row[2]) {
				return true
			}
		}
		return false
	}
	for step := range 2000 {
		var m storage.Mutation
		wantErr := false
		id := int64(r.IntN(12))
		cur, exists := model[id]
		switch op := r.IntN(3); {
		case op == 0 || !exists:
			m = ins(users, randUser(id))
			wantErr = exists || emailTaken(m.New, -1)
		case op == 1:
			m = storage.Mutation{Kind: storage.Update, Rel: users, Old: cur, New: randUser(id)}
			wantErr = emailTaken(m.New, id)
		default:
			m = storage.Mutation{Kind: storage.Delete, Rel: users, Old: cur}
		}
		err := s.Apply(ctx, []storage.Mutation{m})
		if (err != nil) != wantErr {
			t.Fatalf("step %d: %s %v: err=%v wantErr=%v", step, m.Kind, m.New, err, wantErr)
		}
		if err == nil {
			if m.Kind == storage.Delete {
				delete(model, id)
			} else {
				model[id] = m.New
			}
		}
		checkModel(t, s, users, model)
	}
}

func checkModel(t *testing.T, s storage.Store, rel *schema.Relation, model map[int64]value.Row) {
	t.Helper()
	for _, p := range rel.AllPaths() {
		want := make([]value.Row, 0, len(model))
		for _, row := range model {
			want = append(want, row)
		}
		slices.SortFunc(want, func(a, b value.Row) int {
			if c := value.CompareTuples(schema.KeyOf(a, p.KeyCols()), schema.KeyOf(b, p.KeyCols())); c != 0 {
				return c
			}
			return value.CompareTuples(rel.PK(a), rel.PK(b))
		})
		got := scanAll(t, s, storage.ScanRequest{Rel: rel, Path: p})
		if len(got) != len(want) {
			t.Fatalf("path %s: got %d rows, want %d", p.Name, len(got), len(want))
		}
		for i := range got {
			if !got[i].Equal(want[i]) {
				t.Fatalf("path %s row %d: got %v, want %v", p.Name, i, got[i], want[i])
			}
		}
	}
}
