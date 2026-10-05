package graphql_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fpt/go-dquery"
	"github.com/fpt/go-dquery/frontend/graphql"
	"github.com/fpt/go-dquery/frontend/sql"
	"github.com/fpt/go-dquery/internal/fixture"
	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/opt"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/storage/memory"
	"github.com/fpt/go-dquery/value"
)

var ctx = context.Background()

func newSchema(t *testing.T) (*graphql.Schema, *schema.Catalog) {
	t.Helper()
	cat := fixture.Shop()
	s, err := graphql.NewSchema(cat)
	if err != nil {
		t.Fatal(err)
	}
	return s, cat
}

func compile(t *testing.T, s *graphql.Schema, query string, vars map[string]any) *graphql.Operation {
	t.Helper()
	op, errs := s.Compile(graphql.Request{Query: query, Variables: vars}, opt.Options{})
	if len(errs) > 0 {
		t.Fatalf("compile %s: %v", query, errs)
	}
	return op
}

// TestSameIRAsSQL checks the central claim of the design: equivalent SQL
// and GraphQL queries compile to the same optimized plan.
func TestSameIRAsSQL(t *testing.T) {
	s, cat := newSchema(t)
	cases := []struct{ name, sql, gql string }{
		{"latest orders of a user",
			`SELECT id, amount FROM orders WHERE user_id = 1 ORDER BY created_at DESC LIMIT 10`,
			`{ orders(where: {user_id: {_eq: 1}}, order_by: {created_at: desc}, limit: 10) { id amount } }`},
		{"point get",
			`SELECT name FROM users WHERE id = 1`,
			`{ users_by_pk(id: 1) { name } }`},
		{"unique secondary",
			`SELECT id, name FROM users WHERE email = 'ann@x'`,
			`{ users_by_email(email: "ann@x") { id name } }`},
		{"composite key range",
			`SELECT product FROM items WHERE order_id = 10 AND line > 1 ORDER BY line DESC LIMIT 1`,
			`{ items(where: {line: {_gt: 1}, order_id: {_eq: 10}}, order_by: [{line: desc}], limit: 1) { product } }`},
		{"IN list",
			`SELECT id FROM users WHERE id IN (1, 2)`,
			`{ users(where: {id: {_in: [1, 2]}}) { id } }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := sql.Compile(cat, tc.sql, opt.Options{})
			if err != nil {
				t.Fatal(err)
			}
			op := compile(t, s, tc.gql, nil)
			want, got := ir.Explain(c.Plan), ir.Explain(op.Fields[0].Plan)
			if got != want {
				t.Fatalf("plans differ\n--- sql\n%s--- graphql\n%s", want, got)
			}
		})
	}
}

// TestNestedMatchesSQLJoin checks that a nested GraphQL selection uses the
// same access (path, direction, limit) as the equivalent SQL join, while
// keeping the nested shape (Map) instead of flattening.
func TestNestedMatchesSQLJoin(t *testing.T) {
	s, cat := newSchema(t)
	c, err := sql.Compile(cat, `SELECT o.id FROM users u JOIN orders o ON o.user_id = u.id
		WHERE u.id = 1 ORDER BY o.created_at DESC LIMIT 2`, opt.Options{})
	if err != nil {
		t.Fatal(err)
	}
	op := compile(t, s, `{ users_by_pk(id: 1) { name orders(order_by: {created_at: desc}, limit: 2) { id } } }`, nil)
	plan := op.Fields[0].Plan

	want := `Project [users.name, orders: #0]
  Map
    Get users via primary key=[1] cols=[id, name]
    field #0 (many, batched)
      Project [orders.id]
        Scan orders via by_user eq=[$.users.id] reverse limit=2 cols=[id]
`
	if got := ir.Explain(plan); got != want {
		t.Fatalf("graphql plan\n--- got\n%s--- want\n%s", got, want)
	}
	lookup := c.Plan.(*ir.Project).Input.(*ir.Lookup)
	scan := ir.BatchLeaf(plan.(*ir.Project).Input.(*ir.Map).Fields[0].Plan).(*ir.Scan)
	if scan.Path != lookup.Path || scan.Reverse != lookup.Reverse || scan.Limit != lookup.Limit {
		t.Fatalf("access differs: sql %s via %s rev=%v limit=%d, graphql via %s rev=%v limit=%d",
			lookup.Rel.Name, lookup.Path.Name, lookup.Reverse, lookup.Limit, scan.Path.Name, scan.Reverse, scan.Limit)
	}
}

const seed = `
INSERT INTO users (id, name, email) VALUES (1, 'ann', 'ann@x'), (2, 'bob', NULL), (3, 'cat', 'cat@x');
INSERT INTO orders VALUES
  (10, 1, 'open', 5, '2026-01-02'),
  (11, 1, 'paid', 7, '2026-01-03'),
  (12, 1, 'open', 9, '2026-01-04'),
  (13, 2, 'open', 11, '2026-01-05');
INSERT INTO items VALUES (10, 1, 'pen', 2), (10, 2, 'ink', 1), (12, 1, 'pad', 3);
`

type fixtureEnv struct {
	s     *graphql.Schema
	db    *dquery.DB
	store *countingStore
}

func setup(t *testing.T) *fixtureEnv {
	t.Helper()
	s, cat := newSchema(t)
	store := &countingStore{Store: memory.NewStore()}
	db := dquery.Open(cat, store)
	if _, err := db.ExecScript(ctx, seed); err != nil {
		t.Fatal(err)
	}
	store.reset()
	return &fixtureEnv{s: s, db: db, store: store}
}

func (e *fixtureEnv) run(query string, vars map[string]any) *graphql.Response {
	return e.s.Execute(ctx, e.db.Executor, graphql.Request{Query: query, Variables: vars}, opt.Options{})
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func expectData(t *testing.T, resp *graphql.Response, want string) {
	t.Helper()
	if len(resp.Errors) > 0 {
		t.Fatalf("errors: %v", resp.Errors)
	}
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad expectation: %v", err)
	}
	if got, want := toJSON(t, resp.Data), toJSON(t, w); got != want {
		// Key order matters in GraphQL; compare the raw text as well.
		t.Fatalf("data mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestExecuteQuery(t *testing.T) {
	e := setup(t)
	resp := e.run(`
query Shop($uid: Int!, $withEmail: Boolean = false) {
  users_by_pk(id: $uid) {
    name
    email @include(if: $withEmail)
    orders(order_by: {created_at: desc}) { id amount items { ...line } }
  }
  open: orders(where: {status: {_eq: "open"}}, order_by: {created_at: asc}, limit: 2) {
    id
    user { name }
    __typename
  }
  nobody: users_by_pk(id: 99) { name }
  __typename
}
fragment line on Items { product qty }`, map[string]any{"uid": 1})
	// Map key order is not preserved by encoding/json on the expected side,
	// so compare against the exact text produced.
	got := toJSON(t, resp)
	want := `{"data":{"users_by_pk":{"name":"ann","orders":[` +
		`{"id":12,"amount":9,"items":[{"product":"pad","qty":3}]},` +
		`{"id":11,"amount":7,"items":[]},` +
		`{"id":10,"amount":5,"items":[{"product":"pen","qty":2},{"product":"ink","qty":1}]}]},` +
		`"open":[{"id":10,"user":{"name":"ann"},"__typename":"Orders"},{"id":12,"user":{"name":"ann"},"__typename":"Orders"}],` +
		`"nobody":null,"__typename":"Query"}}`
	if got != want {
		t.Fatalf("response\n got: %s\nwant: %s", got, want)
	}
}

func TestExecuteMutations(t *testing.T) {
	e := setup(t)
	resp := e.run(`mutation {
  insert_users(objects: [{id: 4, name: "dan", email: "dan@x"}, {id: 5, name: "eve"}]) {
    affected_rows
    returning { id name email }
  }
  update_orders(where: {user_id: {_eq: 1}, status: {_eq: "open"}}, _set: {status: "paid"}) {
    affected_rows
  }
  delete_items(where: {order_id: {_eq: 10}}) { affected_rows returning { line } __typename }
}`, nil)
	got := toJSON(t, resp)
	want := `{"data":{"insert_users":{"affected_rows":2,"returning":[{"id":4,"name":"dan","email":"dan@x"},{"id":5,"name":"eve","email":null}]},` +
		`"update_orders":{"affected_rows":2},` +
		`"delete_items":{"affected_rows":2,"returning":[{"line":1},{"line":2}],"__typename":"Items_mutation_response"}}}`
	if got != want {
		t.Fatalf("response\n got: %s\nwant: %s", got, want)
	}
	expectData(t, e.run(`{ orders(where: {status: {_eq: "paid"}}) { id } }`, nil), `{"orders":[{"id":10},{"id":11},{"id":12}]}`) // by_status order: created_at
}

func TestErrors(t *testing.T) {
	e := setup(t)
	cases := map[string]string{
		`{ users { nope } }`:                                                                       "Cannot query field",
		`{ orders(where: {amount: {_gt: 1}}) { id } }`:                                             "full scan",
		`{ orders(where: {user_id: {_eq: 1}}, order_by: {amount: asc}) { id } }`:                   "no access path provides the requested order",
		`{ __schema { types { name } } }`:                                                          "introspection",
		`mutation { insert_users(objects: [{id: 9}]) { affected_rows } }`:                          "name",
		`mutation { insert_users(objects: [{id: 1, name: "dup"}]) { affected_rows } }`:             "duplicate key",
		`mutation { insert_users(objects: [{id: 9, name: "x"}]) { returning { orders { id } } } }`: "not supported",
		`{ users_by_pk(id: "x") { name } }`:                                                        "",
	}
	for q, msg := range cases {
		resp := e.run(q, nil)
		if len(resp.Errors) == 0 {
			t.Errorf("%s: expected an error", q)
			continue
		}
		if resp.Data != nil {
			t.Errorf("%s: data should be null on error", q)
		}
		if !strings.Contains(resp.Errors.Error(), msg) {
			t.Errorf("%s: error %q lacks %q", q, resp.Errors.Error(), msg)
		}
	}
	if _, errs := e.s.Compile(graphql.Request{Query: `{ orders(where: {amount: {_gt: 1}}) { id } }`}, opt.Options{}); len(errs) == 0 || !errors.Is(errs[0], opt.ErrFullScan) {
		t.Fatalf("expected wrapped ErrFullScan, got %v", errs)
	}
}

// TestNPlusOneBatching checks that a nested many-to-one field is fetched
// with one GetMany per batch instead of one Get per parent row.
func TestNPlusOneBatching(t *testing.T) {
	e := setup(t)
	expectData(t, e.run(`{ orders(where: {status: {_eq: "open"}}) { id user { name } } }`, nil),
		`{"orders":[{"id":10,"user":{"name":"ann"}},{"id":12,"user":{"name":"ann"}},{"id":13,"user":{"name":"bob"}}]}`)
	if gets, many := e.store.count("Get", "users"), e.store.count("GetMany", "users"); gets != 0 || many != 1 {
		t.Fatalf("users fetched with %d Get and %d GetMany calls; want 0 and 1", gets, many)
	}
}

func TestHTTPHandler(t *testing.T) {
	e := setup(t)
	srv := httptest.NewServer(e.s.Handler(e.db.Executor, opt.Options{}))
	defer srv.Close()

	body := `{"query":"query($id: Int!) { users_by_pk(id: $id) { name } }","variables":{"id":2}}`
	res, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || toJSON(t, out) != `{"data":{"users_by_pk":{"name":"bob"}}}` {
		t.Fatalf("status %d: %v", res.StatusCode, out)
	}

	res, err = http.Get(srv.URL + "?query=" + "mutation%7Bdelete_users(where%3A%7Bid%3A%7B_eq%3A1%7D%7D)%7Baffected_rows%7D%7D")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET mutation: status %d", res.StatusCode)
	}
}

// countingStore records store calls per method and relation.
type countingStore struct {
	storage.Store
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingStore) add(method, rel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[method+" "+rel]++
}

func (c *countingStore) count(method, rel string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method+" "+rel]
}

func (c *countingStore) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

func (c *countingStore) Get(ctx context.Context, rel *schema.Relation, path *schema.AccessPath, key value.Tuple, cols storage.ColSet) (value.Row, bool, error) {
	c.add("Get", rel.Name)
	return c.Store.Get(ctx, rel, path, key, cols)
}

func (c *countingStore) GetMany(ctx context.Context, rel *schema.Relation, path *schema.AccessPath, keys []value.Tuple, cols storage.ColSet) ([]value.Row, error) {
	c.add("GetMany", rel.Name)
	return c.Store.GetMany(ctx, rel, path, keys, cols)
}
