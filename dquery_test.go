package dquery_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fpt/go-dquery"
	"github.com/fpt/go-dquery/exec"
	"github.com/fpt/go-dquery/internal/fixture"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/storage/memory"
	"github.com/fpt/go-dquery/value"
)

var ctx = context.Background()

const seedSQL = `
INSERT INTO users (id, name, email) VALUES (1, 'ann', 'ann@x'), (2, 'bob', NULL), (3, 'cat', 'cat@x');
INSERT INTO orders VALUES
  (10, 1, 'open', 5, '2026-01-02'),
  (11, 1, 'paid', 7, '2026-01-03'),
  (12, 1, 'open', 9, '2026-01-04'),
  (13, 2, 'open', 11, '2026-01-05');
INSERT INTO items VALUES (10, 1, 'pen', 2), (10, 2, 'ink', 1), (12, 1, 'pad', 3);
`

func open(t *testing.T) *dquery.DB {
	t.Helper()
	db := dquery.Open(fixture.Shop(), memory.NewStore())
	if _, err := db.ExecScript(ctx, seedSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

func render(r *exec.Result) string {
	var b strings.Builder
	b.WriteString(strings.Join(r.Columns, " | "))
	b.WriteByte('\n')
	for _, row := range r.Rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = v.String()
		}
		b.WriteString(strings.Join(parts, " | "))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestSQLEndToEnd(t *testing.T) {
	db := open(t)
	steps := []struct {
		sql      string
		params   []value.Value
		want     string
		affected int64
	}{
		{sql: `SELECT u.name, o.id, i.product, i.qty
FROM users u JOIN orders o ON o.user_id = u.id JOIN items i ON i.order_id = o.id
WHERE u.email = 'ann@x'`, want: `
name | id | product | qty
"ann" | 10 | "pen" | 2
"ann" | 10 | "ink" | 1
"ann" | 12 | "pad" | 3
`},
		{sql: `SELECT id, amount FROM orders WHERE user_id = ? ORDER BY created_at DESC LIMIT 2`,
			params: []value.Value{value.Int(1)}, want: `
id | amount
12 | 9
11 | 7
`},
		{sql: `SELECT o.id, i.line FROM orders o LEFT JOIN items i ON i.order_id = o.id WHERE o.id IN (11, 12)`, want: `
id | line
11 | NULL
12 | 1
`},
		{sql: `SELECT o.id, u.name FROM orders o JOIN users u ON u.id = o.user_id
WHERE o.status = 'open' AND o.created_at >= '2026-01-04' ORDER BY o.created_at`, want: `
id | name
12 | "ann"
13 | "bob"
`},
		{sql: `UPDATE orders SET amount = amount * 2 WHERE user_id = 1 AND status = 'open' RETURNING id, amount`,
			affected: 2, want: `
id | amount
10 | 10
12 | 18
`},
		{sql: `INSERT INTO users VALUES (2, 'bobby', NULL), (4, 'dan', 'dan@x')
ON CONFLICT (id) DO UPDATE SET name = excluded.name RETURNING id, name`, affected: 2, want: `
id | name
2 | "bobby"
4 | "dan"
`},
		{sql: `INSERT INTO users VALUES (1, 'dup', NULL) ON CONFLICT DO NOTHING`, affected: 0, want: "\n\n"}, // no columns, no rows
		{sql: `DELETE FROM orders WHERE user_id = 1 AND status = 'paid' RETURNING id`, affected: 1, want: `
id
11
`},
		{sql: `SELECT id, status FROM orders WHERE status = 'open' ORDER BY created_at`, want: `
id | status
10 | "open"
12 | "open"
13 | "open"
`},
		{sql: `SELECT name, email FROM users WHERE email = $1`, params: []value.Value{value.String("dan@x")}, want: `
name | email
"dan" | "dan@x"
`},
	}
	for i, s := range steps {
		res, err := db.Exec(ctx, s.sql, s.params...)
		if err != nil {
			t.Fatalf("step %d %q: %v", i, s.sql, err)
		}
		if got, want := render(res), strings.TrimPrefix(s.want, "\n"); got != want {
			t.Fatalf("step %d %q\n--- got\n%s--- want\n%s", i, s.sql, got, want)
		}
		if res.RowsAffected != s.affected {
			t.Fatalf("step %d: rows affected %d, want %d", i, res.RowsAffected, s.affected)
		}
	}
}

func TestSQLConstraintErrors(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(ctx, `INSERT INTO users VALUES (5, 'eve', 'ann@x')`); !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("unique email: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users (id) VALUES (6)`); err == nil {
		t.Fatal("expected NOT NULL violation")
	}
	if _, err := db.Exec(ctx, `UPDATE users SET id = 9 WHERE id = 1`); err == nil {
		t.Fatal("expected primary key update to fail")
	}
	// Nothing above was applied.
	res, err := db.Exec(ctx, `SELECT id FROM users WHERE id IN (5, 6, 9)`)
	if err != nil || len(res.Rows) != 0 {
		t.Fatalf("partial writes: %v %v", res, err)
	}
}

func TestSQLExplain(t *testing.T) {
	db := open(t)
	res, err := db.Exec(ctx, `EXPLAIN SELECT name FROM users WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	want := "plan\n\"Project [users.name]\"\n\"  Get users via primary key=[1] cols=[name]\"\n"
	if got := render(res); got != want {
		t.Fatalf("got\n%s", got)
	}
	// EXPLAIN of a mutation does not execute it.
	if _, err := db.Exec(ctx, `EXPLAIN DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if res, _ := db.Exec(ctx, `SELECT id FROM users WHERE id = 1`); len(res.Rows) != 1 {
		t.Fatal("EXPLAIN DELETE deleted the row")
	}
}
