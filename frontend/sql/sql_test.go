package sql_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/fpt/go-dquery/frontend/sql"
	"github.com/fpt/go-dquery/internal/fixture"
	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/opt"
)

func TestParseErrors(t *testing.T) {
	cases := map[string]error{
		"SELECT count(*) FROM users":                          sql.ErrNotSupported,
		"SELECT DISTINCT name FROM users":                     sql.ErrNotSupported,
		"SELECT name FROM users GROUP BY name":                sql.ErrNotSupported,
		"SELECT * FROM users, orders":                         sql.ErrNotSupported,
		"SELECT * FROM users u RIGHT JOIN orders o ON true":   sql.ErrNotSupported,
		"SELECT * FROM users LIMIT 1 OFFSET 2":                sql.ErrNotSupported,
		"SELECT * FROM users WHERE id IN (SELECT 1)":          sql.ErrNotSupported,
		"SELECT * FROM (SELECT 1) t":                          sql.ErrNotSupported,
		"SELECT CASE WHEN true THEN 1 END FROM users":         sql.ErrNotSupported,
		"INSERT INTO users SELECT * FROM users":               sql.ErrNotSupported,
		"UPDATE orders SET status = 'x' FROM users":           sql.ErrNotSupported,
		"WITH x AS (SELECT 1) SELECT 1":                       sql.ErrNotSupported,
		"SELECT * FROM users WHERE id = ? AND name = $2":      nil, // syntax error
		"SELECT * FROM users WHERE name = 'unterminated":      nil,
		"SELECT * FROM users WHERE":                           nil,
		"SELECT * FROM users LIMIT $1":                        nil,
		"SELECT * users":                                      nil,
		"SELECT * FROM users; garbage":                        nil,
		"INSERT INTO users (id) VALUES (1) ON CONFLICT DO UP": nil,
	}
	for src, want := range cases {
		_, err := sql.ParseAll(src)
		if err == nil {
			t.Errorf("%q: expected error", src)
			continue
		}
		var syn *sql.SyntaxError
		if want == nil && !errors.As(err, &syn) {
			t.Errorf("%q: expected syntax error, got %v", src, err)
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("%q: got %v, want %v", src, err, want)
		}
	}
}

func TestParseScriptAndComplete(t *testing.T) {
	stmts, err := sql.ParseAll(`
		-- comment; with semicolon
		INSERT INTO users VALUES (1, 'it''s; fine', NULL);
		SELECT "name" FROM users WHERE id = $1;;
		EXPLAIN DELETE FROM users WHERE id = ?`)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 3 {
		t.Fatalf("got %d statements", len(stmts))
	}
	ins := stmts[0].(*sql.Insert)
	if got := ins.Rows[0][1].String(); got != `"it's; fine"` {
		t.Fatalf("string literal: %s", got)
	}
	if _, ok := stmts[2].(*sql.Explain).Stmt.(*sql.Delete); !ok {
		t.Fatalf("explain: %#v", stmts[2])
	}

	for src, want := range map[string]bool{
		"SELECT 1 FROM t;":         true,
		"SELECT 1 FROM t":          false,
		"SELECT ';' FROM t":        false,
		"SELECT 1 FROM t; -- x":    true,
		"SELECT 'a;":               false,
		"SELECT 1 FROM t -- tail;": false,
	} {
		if got := sql.Complete(src); got != want {
			t.Errorf("Complete(%q) = %v, want %v", src, got, want)
		}
	}
}

func TestParseExpressions(t *testing.T) {
	cases := map[string]string{
		"a = 1 OR b = 2 AND c = 3":        "(a = 1) OR ((b = 2) AND (c = 3))",
		"NOT a = 1":                       "NOT (a = 1)",
		"a + b * 2 - -3":                  "(a + (b * 2)) - -3",
		"x BETWEEN 1 AND 5":               "(x >= 1) AND (x <= 5)",
		"x NOT IN (1, 2)":                 "NOT (x IN (1, 2))",
		"t.x IS NOT NULL":                 "t.x IS NOT NULL",
		"x >= ? AND y <> ?":               "(x >= $1) AND (y <> $2)",
		"-(a)":                            "0 - a",
		"\"Quoted\" = 'it''s' ":           `Quoted = "it's"`,
		"TRUE AND x = NULL AND f = 1.5e2": "true AND (x = NULL) AND (f = 150)",
	}
	for src, want := range cases {
		stmt, err := sql.Parse("SELECT 1 FROM t WHERE " + src)
		if err != nil {
			t.Errorf("%q: %v", src, err)
			continue
		}
		if got := stmt.(*sql.Select).Where.String(); got != want {
			t.Errorf("%q: got %s, want %s", src, got, want)
		}
	}
}

func TestCompileGolden(t *testing.T) {
	cat := fixture.Shop()
	cases := []struct {
		name, src, want string
	}{
		{"point get with projection", `SELECT name FROM users WHERE id = 1`, `
Project [users.name]
  Get users via primary key=[1] cols=[name]`},
		{"memo example: join through unique secondary", `
SELECT a.name, b.amount
FROM users a JOIN orders b ON a.id = b.user_id
WHERE a.email = 'ann@x'`, `
Project [a.name, b.amount]
  Lookup orders b via by_user key=[a.id] many cols=[amount]
    Get users a via by_email key=["ann@x"] cols=[id, name]`},
		{"memo example: latest orders", `
SELECT * FROM orders WHERE user_id = ? ORDER BY created_at DESC LIMIT 10`, `
Project [orders.id, orders.user_id, orders.status, orders.amount, orders.created_at]
  Scan orders via by_user eq=[$1] reverse limit=10`},
		{"left join N:1 with IN", `
SELECT o.id, u.name FROM orders o LEFT JOIN users u ON u.id = o.user_id WHERE o.id IN (10, 11)`, `
Project [o.id, u.name]
  Lookup users u via primary key=[o.user_id] one optional cols=[name]
    GetMany orders o via primary keys=[[10], [11]] cols=[id, user_id]`},
		{"three-way traversal", `
SELECT u.name, o.id, i.product
FROM users u JOIN orders o ON o.user_id = u.id JOIN items i ON i.order_id = o.id
WHERE u.id = 1`, `
Project [u.name, o.id, i.product]
  Lookup items i via primary key=[o.id] many cols=[product]
    Lookup orders o via by_user key=[u.id] many cols=[id]
      Get users u via primary key=[1] cols=[id, name]`},
		{"join residual stays above lookup", `
SELECT o.id FROM users u JOIN orders o ON o.user_id = u.id AND o.status = 'open' WHERE u.id = $1`, `
Project [o.id]
  Filter o.status = "open"
    Lookup orders o via by_user key=[u.id] many cols=[id, status]
      Get users u via primary key=[$1] cols=[id]`},
		{"order by select alias, BETWEEN dates", `
SELECT created_at AS t FROM orders
WHERE user_id = 1 AND created_at BETWEEN '2026-01-02' AND '2026-01-03'
ORDER BY t`, `
Project [t: orders.created_at]
  Scan orders via by_user eq=[1] lo>="2026-01-02" hi<="2026-01-03" cols=[created_at]`},
		{"update read side", `
UPDATE orders SET status = 'paid' WHERE user_id = $1 AND status = 'open' RETURNING id`, `
Update orders set [status = "paid"] returning [orders.id]
  Filter orders.status = "open"
    Scan orders via by_user eq=[$1]`},
		{"delete by unique secondary", `DELETE FROM users WHERE email = 'a'`, `
Delete users
  Get users via by_email key=["a"]`},
		{"upsert", `
INSERT INTO users (id, name) VALUES (1, 'a') ON CONFLICT (id) DO UPDATE SET name = excluded.name RETURNING *`, `
Insert users cols=[id, name] values=[1, "a"] on-conflict=update [name = excluded.name] returning [users.id, users.name, users.email]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := sql.Compile(cat, tc.src, opt.Options{})
			if err != nil {
				t.Fatal(err)
			}
			want := strings.TrimPrefix(tc.want, "\n") + "\n"
			if got := ir.Explain(c.Plan); got != want {
				t.Fatalf("plan mismatch\n--- logical\n%s--- got\n%s--- want\n%s", ir.Explain(c.Logical), got, want)
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	cat := fixture.Shop()
	cases := map[string]error{
		"SELECT * FROM orders WHERE amount > 5":                                         opt.ErrFullScan,
		"UPDATE orders SET status = 'x'":                                                opt.ErrFullScan,
		"SELECT * FROM orders WHERE user_id = 1 ORDER BY amount":                        opt.ErrOrder,
		"SELECT * FROM users u JOIN orders o ON o.amount = u.id":                        sql.ErrNotSupported,
		"SELECT * FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.amount > 1": sql.ErrNotSupported,
		"INSERT INTO users (id, name) VALUES (1, 'a') ON CONFLICT (name) DO NOTHING":    sql.ErrNotSupported,
		"SELECT * FROM users WHERE id = 1 ORDER BY id + 1":                              sql.ErrNotSupported,
		"SELECT id FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = 1":       nil, // ambiguous
		"SELECT nope FROM users WHERE id = 1":                                           nil,
		"SELECT x.id FROM users WHERE id = 1":                                           nil,
		"SELECT * FROM missing":                                                         nil,
		"SELECT * FROM users u JOIN users u ON u.id = u.id":                             nil,
		"INSERT INTO users VALUES (id, 'a', NULL)":                                      nil,
	}
	for src, want := range cases {
		_, err := sql.Compile(cat, src, opt.Options{})
		if err == nil {
			t.Errorf("%q: expected error", src)
			continue
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("%q: got %v, want %v", src, err, want)
		}
	}
}
