# go-dquery

An OLTP query runtime for key-value stores, written in Go.

go-dquery compiles queries straight into a small DAG IR built around **access
paths** and **key traversal**: `Get`, `Scan`, `Lookup`, `Map`. The queries are
not routed through SQL as an intermediate language. The IR is optimized with a
few targeted rules and executed against ordered KV engines such as Pebble and
RocksDB, or remote stores such as Bigtable and DynamoDB.

```
 SQL ─────┐
 GraphQL ─┼──▶  IR (DAG)  ──▶  Optimizer  ──▶  Executor  ──▶  Store
 Go API ──┘                                                    ├─ memory / Pebble (kvstore)
                                                               └─ Bigtable / DynamoDB (planned)
```

The target workload is fetching a small working set: point and multi gets, prefix
and range scans over primary or secondary indexes, 1:N and N:1 traversals, and
transactional inserts, updates, and deletes with automatic index maintenance.
Aggregates, arbitrary sorts, and other OLAP features are out of scope.

See [doc/DESIGN.md](doc/DESIGN.md) for the full design and roadmap.

## Status

Early development. The MVP (phases 0–4) is complete: SQL over in-memory or Pebble storage.

| Phase | Scope | Status |
|---|---|---|
| 0 | Values, key encoding, schema catalog | done |
| 1 | IR, executor, memory backend, mutations with index maintenance | done |
| 2 | Optimizer | done |
| 3 | SQL subset frontend, REPL | done |
| 4 | Pebble backend (MVP) | done |
| 5 | GraphQL frontend | done |
| 6 | Bigtable, DynamoDB | planned |

## Example

Define a schema:

```yaml
relations:
  - name: users
    columns:
      - {name: id, type: int}
      - {name: name, type: string}
      - {name: email, type: string, nullable: true}
    primary: {partition: [id]}
    paths:
      - {name: by_email, partition: [email], unique: true}
  - name: orders
    columns:
      - {name: id, type: int}
      - {name: user_id, type: int}
      - {name: amount, type: float}
      - {name: created_at, type: timestamp}
    primary: {partition: [id]}
    paths:
      - {name: by_user, partition: [user_id], sort: [created_at]}
```

Then build and run a plan. This is the latest 10 orders of one user, which a
frontend would produce from either
`SELECT ... FROM users u JOIN orders o ON o.user_id = u.id WHERE u.id = $1 ORDER BY o.created_at DESC LIMIT 10`
or `user(id: $1) { orders(first: 10, orderBy: CREATED_AT_DESC) { ... } }`:

```go
cat, _ := schema.Load("shop.yaml")
users, orders := cat.Relation("users"), cat.Relation("orders")

plan := &ir.Lookup{
	Input: &ir.Get{Rel: users, Alias: "u", Path: users.Primary, Key: ir.Exprs(ir.P(1))},
	Rel:   orders, Alias: "o", Path: orders.Path("by_user"),
	Key:   ir.Exprs(ir.C("u", "id")), Many: true, Reverse: true, Limit: 10,
}

ex := exec.New(memory.NewStore())
res, err := ex.Execute(ctx, plan, value.Int(42))
```

`ir.Explain(plan)` renders:

```
Lookup orders o via by_user key=[u.id] many reverse limit=10
  Get users u via primary key=[$1]
```

## Try it

```sh
go run ./cmd/dq -schema examples/shop.yaml examples/shop.sql
```

```
dq> SELECT u.name, o.id, o.amount
... FROM users u JOIN orders o ON o.user_id = u.id
... WHERE u.id = 1 ORDER BY o.created_at DESC LIMIT 2;
name | id | amount
-----+----+-------
ann  | 12 | 9
ann  | 11 | 7
(2 rows)
dq> EXPLAIN SELECT * FROM orders WHERE status = 'open' AND amount > 6;
Project [orders.id, orders.user_id, orders.status, orders.amount, orders.created_at]
  Filter orders.amount > 6
    Scan orders via by_status eq=["open"]
dq> SELECT * FROM orders WHERE amount > 6;
ERROR: opt: query requires an unbounded full scan: orders
```

The shell supports `SELECT` with joins, `ORDER BY`, and `LIMIT`, as well as
`INSERT` (including `ON CONFLICT`), `UPDATE`, `DELETE`, `RETURNING`, and
`EXPLAIN`. Queries that would need a full scan or a sort that no index provides
are rejected. Pass `-allow-full-scan` to allow full scans, and pass
`-data ./db` to persist the data in a Pebble database. From Go, use
`dquery.Open(catalog, store).Exec(ctx, sql, params...)`.

## GraphQL

The same catalog is exposed as a Hasura-style GraphQL API, compiled straight to
the same IR as SQL:

```sh
go run ./cmd/dq -schema examples/shop.yaml -http :8080 examples/shop.sql
curl -s localhost:8080/graphql -d '{"query":"{ users_by_pk(id: 1) { name orders(order_by: {created_at: desc}, limit: 2) { id amount } } }"}'
# {"data":{"users_by_pk":{"name":"ann","orders":[{"id":12,"amount":9},{"id":11,"amount":7}]}}}
```

The SDL is served at `/graphql/schema`. These two queries compile to the same
plan, `Scan orders via by_user eq=[1] reverse limit=10`:

```sql
SELECT id, amount FROM orders WHERE user_id = 1 ORDER BY created_at DESC LIMIT 10
```
```graphql
{ orders(where: {user_id: {_eq: 1}}, order_by: {created_at: desc}, limit: 10) { id amount } }
```

## Development

```sh
go test -race ./...
```

Every storage backend must pass the conformance suite in `storage/storagetest`.
The suite includes a randomized test that checks index consistency against a
reference model.
