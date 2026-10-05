# go-dquery Design

Module: `github.com/fpt/go-dquery`

## 1. Goal

go-dquery is a pure-Go **OLTP query runtime** that sits between query frontends
(SQL, later GraphQL and a LINQ-like Go API) and key-value storage engines
(Pebble/RocksDB, Bigtable, DynamoDB).

Frontends compile **directly** into a single DAG IR. The IR is not translated
through SQL. A small rule-based optimizer rewrites the IR, and a batched executor
runs it against storage adapters that report their capabilities.

The target workload is *key/index traversal that fetches a small working set*:

- point get / multi get
- range and prefix scans over a primary or secondary access path
- 1:N and N:1 relation traversal
- predicates, projection, limit
- ordering only when an access path provides it
- transactional mutations (insert / update / delete) with index maintenance

### Non-goals (for now)

- OLAP: aggregates, GROUP BY, arbitrary sorts, hash/merge joins, statistics-based
  cost optimization
- A multi-level IR (logical / physical / execution)
- RocksDB via cgo (planned later as an isolated adapter)
- SQL DDL (schemas are defined through a Go API and a JSON/YAML file)

### MVP success criteria

1. The same read query written in SQL and in GraphQL lowers to an **identical IR**.
2. Reads and mutations give identical results on the `memory` and `pebble`
   backends, as verified by a shared conformance suite.
3. Secondary indexes always stay consistent with the rows after any sequence of
   mutations.

## 2. Architecture

```
 SQL ─────┐
 GraphQL ─┼──▶  IR (DAG)  ──▶  Optimizer  ──▶  Executor  ──▶  Store (capability-based)
 Go API ──┘                                                     ├─ kvstore ─┬─ memory
                                                                │           ├─ pebble
                                                                │           └─ rocksdb (later, cgo)
                                                                ├─ bigtable
                                                                └─ dynamodb
```

Principles:

- **The IR does not model a storage API.** It has no `Seek`/`Next`/`Prev`. Scans are
  described by access path, bounds, direction, and limit.
- **Async/RPC concerns never show up in the IR.** Concurrency is an executor and
  adapter detail, handled with goroutines and `errgroup`.
- **Indexes are access paths in the schema**, not physical engine structures.

## 3. Package layout

```
go-dquery/
  dquery.go              DB: compile SQL → optimize → execute (package dquery)
  cmd/dq/                REPL: load schema, run queries, EXPLAIN
  examples/              example schema and seed data
  value/                 Value, Row, Tuple; order-preserving key encoding
  schema/                Catalog: Relation, Column, AccessPath, Relationship; JSON/YAML loader
  ir/                    Node and expression types, builder, validator, EXPLAIN printer
  opt/                   rewrite rules + access-path selection
  exec/                  executor (batched pull iterators, mutation application)
  frontend/sql/          SQL subset parser + binder → IR
  frontend/graphql/      GraphQL (vektah/gqlparser) → IR
  storage/               Store interface, Capabilities, ScanRequest, Mutation
  storage/kvstore/       relations on top of an ordered KV (row + index encoding, writes)
  storage/memory/        ordered in-memory KV (tidwall/btree), used under kvstore
  storage/pebble/        Pebble, used under kvstore
  storage/storagetest/   conformance suite that every backend must pass
  storage/bigtable/      later
  storage/dynamodb/      later
```

## 4. Values and encoding (`value`)

- `Value`: a tagged union of `Null, Bool, Int64, Float64, String, Bytes, Timestamp`,
  plus `List` and `Record` for nested results.
- `Row`: values in column order, with a reference to the column layout.
- `Tuple`: an ordered list of values, used as a key.

**Key encoding** is order-preserving and modeled on the FoundationDB tuple layer
and CockroachDB key encoding:

- a type tag byte per element, chosen so that NULL sorts first
- ints: sign-flipped big-endian
- floats: IEEE bits, transformed to be order-preserving
- strings and bytes: escaped (`0x00 → 0x00 0xFF`) and terminated with `0x00 0x01`
  so that prefixes are correct
- an encoding for descending columns, by complementing the bytes (reserved for later)

Property tests must show that `compare(a, b) == bytes.Compare(enc(a), enc(b))`
and that prefix tuples encode to byte prefixes.

## 5. Schema (`schema`)

```go
type Relation struct {
    ID      uint32
    Name    string
    Columns []Column
    Primary *AccessPath
    Paths   []*AccessPath   // secondary access paths
}

type AccessPath struct {
    ID        uint32
    Name      string
    Partition []ColID // must be bound by equality (Dynamo partition key / KV prefix)
    Sort      []ColID // may be prefix/range-bound; gives ordering
    Unique    bool
    Primary   bool
}

type Relationship struct {      // used by GraphQL/Go frontends for traversal
    Name     string             // e.g. "orders" on users
    From     RelID; FromCols []ColID
    To       RelID; Path     PathID   // access path used to traverse
    Kind     OneToOne | OneToMany | ManyToOne
}
```

The split into `Partition` and `Sort` keys is what lets one model cover every
backend:

- DynamoDB `Query` requires equality on the partition key, then a range on the sort key.
- Bigtable, Pebble, and RocksDB need only a key prefix. Partition and sort are
  concatenated and an empty partition is allowed.

Schemas are defined through the Go API or loaded from JSON/YAML. There is no DDL.

## 6. IR (`ir`)

The IR is a DAG of immutable nodes, and nodes can share subplans. Every node has
an output shape: `One` (0..1 rows), `Stream`, or `Effect` (mutation result).

**Row layout.** `Get`, `GetMany`, and `Scan` always produce full-width rows of
their relation, with columns qualified by the alias. `Cols` is only a projection
hint to the store and never changes positions. `Lookup` appends the target's
columns to its input row, `Map` appends one value per field, and `Project`
replaces the layout. Expressions refer to columns by `(qualifier, name)`, and the
executor resolves them to positions. Because of this, optimizer rewrites never
need to renumber columns.

### 6.1 Read ops

| Op | Shape | Notes |
|---|---|---|
| `Get(rel, path, key []Expr, cols)` | One | `path` must be unique |
| `GetMany(rel, path, keys Stream, cols)` | Stream | produced mainly by lookup batching |
| `Scan(rel, path, eq []Expr, lo, hi Bound, reverse, limit, cols)` | Stream | no `eq` and no bounds = full scan of the path (allowed on any path) |
| `Filter(in, pred)` | same as in | |
| `Project(in, []NamedExpr)` | same as in | |
| `Lookup(in, rel, path, key []Expr, many, optional, reverse, limit, cols)` | Stream | correlated traversal with flat output (SQL joins). `key` is evaluated against the input row. `optional` = left join |
| `Map(in, []Field{name, subplan, one, batched})` | Stream | nested records (GraphQL-style). `Outer` refs in a subplan bind to the input row |
| `Limit(in, n)` | Stream | |
| `Sort(in, keys)` | same as in | logical only; the optimizer must remove it (§7) |
| `Return(in)` | — | plan root |

### 6.2 Mutation ops

| Op | Notes |
|---|---|
| `Insert(rel, cols, rows [][]Expr \| in Stream, onConflict, returning)` | `onConflict`: nil = error (default), `DoNothing`, or `Set` (upsert on PK). `Set` sees the existing row under the alias and the proposed row under `excluded` |
| `Update(rel, alias, in Stream, set []Assign, returning)` | `in` is a read plan that yields the target rows (it uses the same access-path selection as reads). It must expose every column of `rel` under `alias`, and it can be a `Lookup` chain (`UPDATE ... FROM`) |
| `Delete(rel, alias, in Stream, returning)` | |

`returning` is an optional field on each mutation. It is evaluated on the new row
(insert/update) or the old row (delete).

A mutation plan has three steps:

1. Run the read side to completion. This finds the target rows, deduplicated by
   PK, so a join that reaches a row twice updates it once and there is no
   Halloween problem.
2. Compute new row images. Every `SET` right-hand side sees the old values.
3. Send everything to `Store.Apply` as **one atomic batch**.

The row images are the full old and new rows, so index maintenance never needs
another read. Updating PK columns is rejected in the MVP. If `Apply` fails with
`ErrConflict`, the executor re-runs the whole statement, up to `MaxRetries`
times (default 3).

### 6.3 Expressions

Expressions are literal, parameter (`$1`), column ref, outer ref (`$.col`, inside
`Lookup`/`Map` subplans), comparison, `AND/OR/NOT`, `IN (list)`, `IS NULL`, and
simple arithmetic for `UPDATE ... SET x = x + 1`.

### 6.4 EXPLAIN

A deterministic text printer is built first, because golden tests depend on it.

```
Return
  Lookup orders.by_user key=[$.id] kind=1:N limit=10 cols=[id,amount]
    Get users.primary key=[$1] cols=[id,name]
```

A serializable form (protobuf) is planned later, so that a non-Go executor can be
added.

## 7. Optimizer (`opt`)

`opt.Optimize(plan, Options{AllowFullScan})` makes a single top-down rewrite,
followed by a few bottom-up passes. It never modifies its input.

Frontends may emit naive plans, such as `Filter(Scan full)` with an optional
`Sort` and `Limit`, or plans that are already specific. ORDER BY is expressed as
a logical **`Sort`** node. `Sort` has no runtime implementation: the optimizer
must remove it by choosing a path and direction that provide the order, and the
executor rejects any `Sort` that is left.

1. **Predicate pushdown:** conjuncts of a `Filter` move below `Lookup` when they
   reference no target columns, and below `Map` when they reference no field.
   Stacked filters are merged.
2. **Access-path selection:** for `Filter(Scan full)`, the conjuncts of the form
   `col op bindable` (where bindable means a literal, parameter, outer ref, or
   arithmetic over these) are matched against every path:
   - The equality prefix must cover the partition. A range on the next key
     column becomes `lo`/`hi`.
   - A full unique key becomes `Get`, and `pk IN (...)` on a single-column
     unique path becomes `GetMany`.
   - Literals that cannot be coerced to the key column type are not used as
     keys, because the filter and key semantics would differ.
   - Conjuncts that are not consumed stay in a `Filter`.
3. **Order satisfaction:** `Sort` keys must follow the path's key columns that
   come after the equality prefix, all in one direction. Equality-bound columns
   are skipped, and keys after a full unique key do not matter. A descending
   order sets `reverse`. `Lookup` preserves input order. Ordering by `Lookup`
   target columns is satisfied by the lookup path when the input is a single
   row (`Get`). Anything else fails with `ErrOrder`.
4. **Limit pushdown:** into `Scan.limit`, through `Project`, `Map`, and 1:1
   optional `Lookup`, and into `Lookup.limit` when its input is a single row. A
   `Limit` stays in place above a residual `Filter`.
5. **Projection pushdown:** each `Get`/`GetMany`/`Scan`/`Lookup` gets the columns
   that its consumers reference. Mutations always need full rows.
6. **Map batching (N+1 elimination):** the executor already batches every
   `Lookup`. A N:1 lookup issues one `GetMany` per input batch, and a 1:N lookup
   runs bounded scans in parallel. This rule marks a `Map` field as `batched`
   when its plan is a chain of row-wise operators over one `Get`/`Scan` leaf. The
   executor then fetches the leaf for the whole input batch (one `GetMany`, or
   parallel scans) and runs the rest of the chain per row.
7. **Final check:** a `Scan` with no bounds and no limit fails with `ErrFullScan`
   unless `AllowFullScan` is set.

The same rules plan the read side of `Update` and `Delete`.

**Ranking** (rule-based, with no statistics yet). Ties go to the candidate that
consumes more conjuncts, then to the earlier path (primary first).

| Access | Cost |
|---|---|
| exact primary key (`Get`) | 1 |
| exact key on unique secondary path (`Get`) | 2 |
| `IN` list on single-column unique path (`GetMany`) | 3 |
| equality prefix (with optional range) | 10 |
| range only | 50 |
| full scan | 10000 (refused unless `AllowFullScan` is set, or bounded by a limit) |

Planned: refusing paths that need a capability the store lacks, for example a
range on a path that is not ordered.

## 8. Executor (`exec`)

Execution uses pull iterators that yield batches:

```go
type Stream interface {
    Next(ctx context.Context) (Batch, error) // Batch: up to N rows; io.EOF at end
    Close() error
}
```

- The default batch size is 256 and can be configured.
- `Lookup` and `GetMany` take a whole input batch at a time. When the store has no
  native multi-get, the executor fans out with `errgroup` under a concurrency
  limit.
- `Map` runs its subplans for each input batch. The outer ref `$` is bound to each
  row.
- The public entry point is
  `Execute(ctx, plan, params) (*Result, error)`. Callers never see whether the
  backend is local or uses RPC.

## 9. Storage (`storage`)

```go
type Store interface {
    Capabilities() Capabilities
    Get(ctx context.Context, rel *schema.Relation, path *schema.AccessPath, key value.Tuple, cols ColSet) (value.Row, bool, error)
    GetMany(ctx context.Context, rel *schema.Relation, path *schema.AccessPath, keys []value.Tuple, cols ColSet) ([]value.Row, error) // aligned with keys; nil = missing
    Scan(ctx context.Context, req ScanRequest) (RowIterator, error)
    Apply(ctx context.Context, batch MutationBatch) error // atomic
}

type ScanRequest struct {
    Rel     *schema.Relation
    Path    *schema.AccessPath
    Eq      value.Tuple   // partition + leading sort equality prefix
    Lo, Hi  Bound         // on the next sort column(s); inclusive/exclusive/unbounded
    Reverse bool
    Limit   int           // 0 = none
    Cols    ColSet
}

type Mutation struct {
    Kind     Insert | Update | Delete
    Rel      *schema.Relation
    Old, New value.Row     // full row images (Old nil for insert, New nil for delete)
}
// Insert implies "must not exist" (ErrDuplicateKey). Update/Delete imply "the
// stored row must equal Old" (ErrConflict), i.e. optimistic concurrency.

type Capabilities struct {
    MultiGet, OrderedScan, ReverseScan, ProjectionPushdown bool
    AtomicMultiRow   bool                      // Apply is atomic across rows
    // later: ConditionalWrite, PathConsistency map[PathID]Consistency (Dynamo GSI)
}
```

A scan over a secondary path returns **full rows**. Each adapter decides how:
`kvstore` scans the index and then does a multi-get, while DynamoDB queries the
GSI natively.

### 9.1 `kvstore`: relations over an ordered KV

`memory`, `pebble`, and later `rocksdb` all share this layer. The KV underneath
needs only:

```go
type KV interface {
    Get(key []byte) ([]byte, bool, error)
    NewIter(lo, hi []byte, reverse bool) Iter
    NewBatch() Batch            // Set/Delete; Commit is atomic
}
```

Key layout:

```
row:    <relID:4><primaryPathID:4><enc(pk tuple)>               → enc(row columns)
index:  <relID:4><pathID:4><enc(path cols)><enc(pk tuple)>      → ∅
```

Unique secondary paths use the same index layout. Uniqueness is enforced by a
prefix check under the write lock, and keys that contain NULL never conflict.
When a scan reads an index entry and then fetches the row, it re-derives the
index key from the row and skips the entry if the key no longer matches. This
protects against a concurrent write landing between the two reads.

Write path for each `Apply`, all inside **one KV batch**:

1. Insert: check that the PK does not exist (`MustNotExist`) and that unique paths
   have no conflict. Then write the row and every index entry.
2. Update: write the new row. For each path whose key columns changed, delete the
   old index entry and write the new one. Check unique paths for conflicts.
3. Delete: remove the row and every index entry.

Within one `Apply`, later mutations see the pending writes of earlier ones
through an overlay, so duplicates inside a batch are detected.

Concurrency: in the MVP, `kvstore` serializes `Apply` with a store-wide mutex.
The existence, uniqueness, and `Old`-image checks run under that mutex. This
makes check-then-write correct within one process. If the stored row differs
from `Old` (a lost update between the read side and `Apply`), the result is
`ErrConflict`, and the executor retries the statement.

### 9.2 Pebble and catalog binding

`storage/pebble` adapts Pebble v2 to `kvstore.KV`:
- An iterator uses Pebble's point-in-time view, with bounds set as
  `LowerBound`/`UpperBound`.
- A batch is a Pebble batch, synced by default. `NoSync` skips the fsync.
- `OpenInMemory` uses Pebble's in-memory filesystem, for tests.

Relation and path IDs are part of every key and are assigned by position in the
schema. A persistent store must not be reopened with a schema that changes
them. `kvstore.Store.BindCatalog(cat)` records each relation's layout (ID, name,
columns with type and nullability, and access paths) under a metadata key,
using relation ID 0, which is never assigned to a table. On the next open it
verifies the layout:
- New relations are allowed.
- A changed, reordered, or removed relation is an error.

`dq -data DIR` and every persistent embedding should call `BindCatalog` right
after opening the store.

### 9.3 Remote backends (later)

- **Bigtable:** uses the same key layout as `kvstore`, with one table per database
  and one row per KV entry. Writes are atomic only within a single row, so
  `AtomicMultiRow=false`. Index maintenance is best-effort, with a read-side check
  that drops index hits whose row no longer matches. The check uses
  `CheckAndMutateRow` on the base row.
- **DynamoDB:** each relation maps to a table. Primary access path = (partition
  key, sort key). Secondary paths = GSIs, with `Eventual` consistency. `Apply` maps
  to `TransactWriteItems` with condition expressions, and `GetMany` maps to
  `BatchGetItem`.

When a statement needs a capability that the backend lacks, it fails at plan
time. For example, a multi-row atomic mutation is refused on a store with
`AtomicMultiRow=false` unless the caller opts in.

## 10. Frontends

### 10.1 SQL subset (`frontend/sql`)

The frontend is a hand-written lexer and recursive-descent parser. The parser
produces `ir.Expr` directly, so there is no separate expression AST. The binder
resolves names against the catalog and emits a naive plan, which `opt` then
optimizes.

```
SELECT item, ... FROM t [[AS] a]
       [[INNER] JOIN | LEFT [OUTER] JOIN u [[AS] b] ON cond]...
       [WHERE cond] [ORDER BY col [ASC|DESC], ...] [LIMIT n]
INSERT INTO t [(cols)] VALUES (...), ... [ON CONFLICT [(pk cols)] DO NOTHING | DO UPDATE SET c = e, ...]
UPDATE t [[AS] a] SET c = e, ... [WHERE cond]
DELETE FROM t [[AS] a] [WHERE cond]
INSERT / UPDATE / DELETE ... [RETURNING item, ...]
EXPLAIN <statement>
```

The supported expressions are:
- comparisons, `AND`/`OR`/`NOT`, `[NOT] IN (list)`, `[NOT] BETWEEN`, and
  `IS [NOT] NULL`
- `+ - * / %` and unary `-`
- literals: `'string'` with `''` as the escape, integers, floats, `TRUE`,
  `FALSE`, and `NULL`
- parameters, either `$n` or positional `?`, but not both in one statement
- unquoted identifiers, which are folded to lower case, and `"quoted"`
  identifiers

String literals are coerced to timestamps where needed. The accepted forms are
RFC 3339, `YYYY-MM-DD[ HH:MM:SS]`, and `YYYY-MM-DD`.

Binding:
- Every column reference is qualified (`alias.col`). Ambiguous and unknown
  names are errors.
- `FROM t` becomes `Scan(t, primary)` (full), and `WHERE` becomes a `Filter`.
  The optimizer turns these into access paths.
- **Joins are bound to `Lookup`s from left to right.** Equality conjuncts of
  the form `u.col = <expression over earlier tables>` must bind a key prefix
  that covers the partition of one of `u`'s access paths. A full unique key is
  preferred (`one`); otherwise the longest prefix is used (`many`). The other
  `ON` conjuncts become a `Filter` above the lookup. `LEFT JOIN` becomes an
  optional lookup and does not allow extra `ON` conjuncts.
- `ORDER BY` items must be columns, or select-list aliases of columns, and
  become a `Sort` for the optimizer to satisfy. `LIMIT` takes an integer
  literal.
- `ON CONFLICT` may only target the primary key. In `DO UPDATE SET`, the
  proposed row is available as `excluded`.
- `VALUES` may not reference columns.

Not supported (`ErrNotSupported`): aggregates and functions, `GROUP BY`,
`HAVING`, `DISTINCT`, subqueries, `WITH`, `UNION`/`INTERSECT`/`EXCEPT`,
`OFFSET`, `CASE`, `EXISTS`, comma, `RIGHT`, `FULL`, and `CROSS` joins, joins
that bind no access path, `INSERT ... SELECT`, `UPDATE ... FROM`, and ORDER BY
expressions. The optimizer reports ORDER BY clauses that no index serves
(`ErrOrder`) and unbounded full scans, including `UPDATE`/`DELETE` without a
selective `WHERE` (`ErrFullScan`, unless `AllowFullScan` is set).

**API.** `sql.Compile(catalog, src, opts)` returns the logical plan and the
optimized plan. `dquery.DB` wraps a catalog, an executor, and options:
- `Exec` runs one statement with parameters.
- `ExecScript` runs a script.
- `EXPLAIN` returns the optimized plan as rows.

**REPL.** `dq -schema s.yaml [-data DIR] [-allow-full-scan] [-c SQL] [script.sql ...]`
uses an in-memory store by default, or a Pebble database in `DIR`. Statements end with `;`. The meta-commands are `\d`
(list tables), `\d table` (columns, access paths, and relationships), and `\q`.

### 10.2 GraphQL (`frontend/graphql`, after the MVP core)

The schema is derived from the catalog: relations become types, and
`Relationship`s become fields. Root fields are `rel(pk)` and `rels(where, first,
orderBy)`. Nested selections lower to `Map`/`Lookup`, and `first` lowers to
`limit`. `orderBy` must map to a path ordering.

## 11. Testing strategy

- `value`: property tests (`testing/quick` or `rapid`) for encoding order and prefix
  behavior.
- `ir`/`opt`: golden tests that pair an input plan with its EXPLAIN output. Golden
  files are updated with `-update`.
- `frontend/sql`: golden tests that pair SQL with IR EXPLAIN output.
- `storage/storagetest`: a conformance suite run against every backend. It covers
  CRUD, unique conflicts, index consistency after random mutation sequences
  (checked against a model), scan bounds and direction, and limit.
- End to end: fixture schema `users → orders → order_items`, with random
  mutations, then reads compared against an in-memory reference model.
- Thesis test: a SQL query and its equivalent GraphQL query produce equal IR.

## 12. Roadmap

| Phase | Deliverable | Done when |
|---|---|---|
| 0 ✅ | `go.mod`, `value` + key encoding, `schema` catalog (Go API + JSON/YAML) | encoding property tests pass |
| 1 ✅ | `ir` (read + mutation ops), builder, EXPLAIN; `kvstore` + `memory` with `Apply` and index maintenance; `exec` | hand-built read/mutation plans run; conformance suite (incl. randomized index consistency) passes on memory |
| 2 ✅ | `opt` rules 1–7, also applied to the mutation read side | golden plan tests; optimized vs. original result equivalence |
| 3 ✅ | SQL subset including INSERT/UPDATE/DELETE/RETURNING/ON CONFLICT; `cmd/dq` REPL | SQL golden tests; end-to-end tests on memory |
| 4 ✅ | `pebble` backend; full conformance suite incl. randomized index-consistency test; catalog binding; `dq -data` | **MVP:** memory and pebble pass identical suites, and the end-to-end SQL tests run on both |
| 5 | GraphQL frontend; N+1 batching verified | thesis test passes |
| 6 | Bigtable (`cbtemulator`) and DynamoDB (DynamoDB Local) adapters | conformance suite passes, with capability-based skips |
| later | protobuf IR serialization, RocksDB cgo adapter, multi-statement transactions, LINQ-like Go API, descending-column encoding | |

## 13. Open questions

- **Multi-statement transactions:** a `Txn` interface with read-your-writes over
  Pebble indexed batches, versus single-statement atomicity only. The MVP does
  single-statement only.
- **Nested result representation:** nested results are `Record`/`List` values
  inside `Row`. Is a separate result tree needed for GraphQL errors and nullability?
- **Index entries that cover extra columns:** this would avoid the fetch after an
  index scan, at the cost of write amplification.
- **Schema evolution** (adding columns or paths, backfilling indexes): out of scope
  for the MVP. `BindCatalog` refuses incompatible changes instead of misreading
  data. Only adding a relation is allowed.
