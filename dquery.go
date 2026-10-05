// Package dquery ties the pieces together: it compiles SQL against a
// catalog, optimizes it, and executes it on a store.
package dquery

import (
	"context"
	"strings"

	"github.com/fpt/go-dquery/exec"
	"github.com/fpt/go-dquery/frontend/sql"
	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/opt"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/value"
)

// DB executes SQL statements. It is safe for concurrent use.
type DB struct {
	Catalog  *schema.Catalog
	Executor *exec.Executor
	Options  opt.Options
}

// Open returns a DB over store using catalog.
func Open(catalog *schema.Catalog, store storage.Store) *DB {
	return &DB{Catalog: catalog, Executor: exec.New(store)}
}

// Exec runs one statement. For EXPLAIN, the result has a single "plan"
// column with one row per plan line.
func (db *DB) Exec(ctx context.Context, src string, params ...value.Value) (*exec.Result, error) {
	stmt, err := sql.Parse(src)
	if err != nil {
		return nil, err
	}
	return db.ExecStmt(ctx, stmt, params...)
}

// ExecStmt runs a parsed statement.
func (db *DB) ExecStmt(ctx context.Context, stmt sql.Statement, params ...value.Value) (*exec.Result, error) {
	c, err := sql.CompileStmt(db.Catalog, stmt, db.Options)
	if err != nil {
		return nil, err
	}
	if c.Explain {
		return explainResult(c.Plan), nil
	}
	return db.Executor.Execute(ctx, c.Plan, params...)
}

// ExecScript runs a semicolon-separated script without parameters and
// returns one result per statement. It stops at the first error.
func (db *DB) ExecScript(ctx context.Context, src string) ([]*exec.Result, error) {
	stmts, err := sql.ParseAll(src)
	if err != nil {
		return nil, err
	}
	var out []*exec.Result
	for _, s := range stmts {
		res, err := db.ExecStmt(ctx, s)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

func explainResult(plan ir.Node) *exec.Result {
	res := &exec.Result{Columns: []string{"plan"}}
	for _, line := range strings.Split(strings.TrimRight(ir.Explain(plan), "\n"), "\n") {
		res.Rows = append(res.Rows, value.Row{value.String(line)})
	}
	return res
}
