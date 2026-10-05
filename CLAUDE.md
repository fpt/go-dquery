# CLAUDE.md

Guidance for working in this repository.

## Project

go-dquery is a pure-Go OLTP query runtime. Frontends (SQL, later GraphQL) compile
directly into a single DAG IR. An optimizer rewrites it, and an executor runs it
against capability-based storage adapters (memory and Pebble now; Bigtable and
DynamoDB later). `doc/DESIGN.md` is the source of truth for architecture and the
roadmap. Read it before making structural changes, and update it (including the
roadmap status) when a design decision changes.

## Commands

```sh
go build ./...
go vet ./...
go test ./...               # all tests
go test -race ./...         # run before finishing any change touching exec/ or storage/
go test ./storage/... -run TestConformance/RandomizedIndexConsistency -v
gofmt -l .                  # must print nothing
```

## Layout

- `value/` — Value/Row/Tuple and the order-preserving key encoding. Encoding changes must keep the property tests green.
- `schema/` — Catalog: relations, columns, access paths (`Partition` + `Sort` columns), relationships. Built from `CatalogDef` (Go) or YAML/JSON.
- `ir/` — IR nodes and expressions, `Validate`, `Explain` (deterministic; used by golden tests).
- `opt/` — Rule-based optimizer (access-path selection, predicate/limit/projection pushdown, order satisfaction, Map batching).
- `exec/` — Batched pull executor; mutations run the read side, then one atomic `Store.Apply`.
- `storage/` — `Store` interface and capabilities. `storage/kvstore` implements relations on an ordered KV (row/index encoding, index maintenance). `storage/memory` (in-memory) and `storage/pebble` (persistent; `BindCatalog` guards the on-disk layout) are KV engines. `storage/storagetest` is the conformance suite.
- `frontend/sql/` — SQL subset: lexer, recursive-descent parser (produces `ir.Expr` directly), binder (name resolution; JOIN ... ON → `Lookup` on an access path). `sql.Compile` = parse + bind + `opt.Optimize`.
- `dquery.go` (root package `dquery`) — `DB`: SQL in, results out (`Exec`, `ExecScript`, EXPLAIN as rows).
- `cmd/dq/` — REPL over an in-memory store (`go run ./cmd/dq -schema examples/shop.yaml examples/shop.sql`).
- `examples/` — Example schema and seed data. `examples/shop.yaml` must stay identical to `internal/fixture` (a test enforces this).
- `internal/fixture/` — Shared `users → orders → items` test schema and row helpers.

## Invariants and conventions

- IR nodes are immutable. Optimizer rules copy a node and change the copy. They never mutate the input plan.
- Rows from `Get`/`GetMany`/`Scan` are always full width, in relation column order. `Cols` is only a projection hint. Expressions refer to columns by `(qualifier, name)` and are resolved by the executor, so rewrites never renumber columns.
- The IR is not a storage API. Do not add `Seek`/`Next`-style ops. Storage-specific behavior belongs in adapters and is exposed through `storage.Capabilities`.
- No async in the IR or `Store` interface. Concurrency lives in the executor and adapters (`errgroup`).
- OLTP only. A `Sort` that no access path satisfies is an optimizer error, not a runtime sort. Unbounded full scans are rejected unless `AllowFullScan` is set.
- Mutations: inserts must not exist yet, and updates/deletes carry the full `Old` image (optimistic check → `ErrConflict`, retried by the executor). Primary key columns cannot be updated.
- Every new storage backend must pass `storagetest.Run`.
- SQL outside the OLTP subset must fail with `sql.ErrNotSupported` naming the construct. Do not silently fall back to full scans or runtime sorts.
- Tests: prefer table-driven tests with golden EXPLAIN strings for plans and rendered result tables for execution. Use `internal/fixture` instead of ad-hoc schemas.
- Match existing style: short doc comments on exported identifiers, errors prefixed with the package name (`exec: ...`, `kvstore: ...`), and sentinel errors wrapped with `%w`.
