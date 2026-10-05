package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fpt/go-dquery"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage/memory"
)

func TestShellSession(t *testing.T) {
	in := strings.NewReader(`
SELECT u.name, o.id
FROM users u JOIN orders o ON o.user_id = u.id
WHERE u.id = 1 ORDER BY o.created_at DESC LIMIT 2;
UPDATE users SET name = 'Ann' WHERE id = 1 RETURNING id, name;
SELECT * FROM orders WHERE amount > 6;
\d
\nope
SELECT name FROM users WHERE id = 1
`)
	var out, errOut bytes.Buffer
	code := run([]string{"-schema", "../../examples/shop.yaml", "../../examples/shop.sql"}, in, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	want := `OK, 3 rows affected
OK, 4 rows affected
OK, 3 rows affected
name | id
-----+---
ann  | 12
ann  | 11
(2 rows)
id | name
---+-----
1  | Ann
OK, 1 row affected
table
------
items
orders
users
name
----
Ann
(1 row)
`
	if got := out.String(); got != want {
		t.Fatalf("stdout\n--- got\n%s--- want\n%s", got, want)
	}
	for _, msg := range []string{"unbounded full scan", `unknown command \nope`} {
		if !strings.Contains(errOut.String(), msg) {
			t.Errorf("stderr lacks %q:\n%s", msg, errOut.String())
		}
	}
}

func TestCommandFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"-schema", "../../examples/shop.yaml", "-allow-full-scan",
		"-c", "INSERT INTO users VALUES (1, 'a', NULL); SELECT name FROM users WHERE name = 'a'"}, nil, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "(1 row)") {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
	}
	if code := run([]string{"-schema", "../../examples/shop.yaml", "-c", "SELECT nope"}, nil, &out, &errOut); code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if code := run(nil, nil, &out, &errOut); code != 2 {
		t.Fatalf("missing -schema: expected exit 2, got %d", code)
	}
}

func TestPersistentData(t *testing.T) {
	dir := t.TempDir()
	args := func(extra ...string) []string {
		return append([]string{"-schema", "../../examples/shop.yaml", "-data", dir}, extra...)
	}
	var out, errOut bytes.Buffer
	if code := run(args("-c", "INSERT INTO users VALUES (7, 'gus', 'gus@x')"), nil, &out, &errOut); code != 0 {
		t.Fatalf("insert: exit %d: %s", code, errOut.String())
	}
	out.Reset()
	if code := run(args("-c", "SELECT name FROM users WHERE email = 'gus@x'"), nil, &out, &errOut); code != 0 {
		t.Fatalf("select: exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "gus") {
		t.Fatalf("data not persisted:\n%s", out.String())
	}
}

func TestGraphQLMux(t *testing.T) {
	var out, errOut bytes.Buffer
	cat, err := schema.Load("../../examples/shop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sh := &shell{db: dquery.Open(cat, memory.NewStore()), out: &out, errOut: &errOut}
	sh.execAll(`INSERT INTO users VALUES (1, 'ann', NULL);`)
	mux, err := newMux(sh.db)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/graphql", "application/json", strings.NewReader(`{"query":"{ users_by_pk(id: 1) { name } }"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), `{"data":{"users_by_pk":{"name":"ann"}}}`) {
		t.Fatalf("graphql: %s", body)
	}
	res, err = http.Get(srv.URL + "/graphql/schema")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), "users_by_pk(id: Int!): Users") {
		t.Fatalf("schema: %.200s", body)
	}

	sh.meta([]string{`\graphql`})
	if !strings.Contains(out.String(), "type Mutation {") {
		t.Fatalf("\\graphql output: %.200s", out.String())
	}
}
