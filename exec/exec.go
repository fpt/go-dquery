// Package exec executes IR plans against a storage.Store.
//
// Read plans are compiled into a tree of batched pull iterators. Mutation
// plans run their read side to completion, compute full row images, and
// apply all changes in one atomic Store.Apply call.
package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/fpt/go-dquery/ir"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/value"
)

// Executor runs plans. It is safe for concurrent use.
type Executor struct {
	Store storage.Store
	// BatchSize is the maximum number of rows per batch (default 256).
	BatchSize int
	// Concurrency bounds parallel store calls within one operator (default 16).
	Concurrency int
	// MaxRetries is how many times a mutation is re-run after a write
	// conflict (default 3).
	MaxRetries int
}

func New(store storage.Store) *Executor {
	return &Executor{Store: store, BatchSize: 256, Concurrency: 16, MaxRetries: 3}
}

// Result is the outcome of a plan.
type Result struct {
	Columns      []string
	Rows         []value.Row
	RowsAffected int64 // mutations only
}

// env carries per-execution state.
type env struct {
	ex     *Executor
	params []value.Value
	outer  value.Row // enclosing row inside Map subplans
}

// Execute validates and runs a plan.
func (x *Executor) Execute(ctx context.Context, plan ir.Node, params ...value.Value) (*Result, error) {
	if err := ir.Validate(plan); err != nil {
		return nil, err
	}
	if r, ok := plan.(*ir.Return); ok {
		plan = r.Input
	}
	e := &env{ex: x, params: params}
	if plan.Shape() == ir.ShapeEffect {
		for attempt := 0; ; attempt++ {
			res, err := x.execMutation(ctx, plan, e)
			if errors.Is(err, storage.ErrConflict) && attempt < x.MaxRetries {
				continue
			}
			return res, err
		}
	}
	o, err := compile(plan, nil)
	if err != nil {
		return nil, err
	}
	rows, err := run(ctx, o, e)
	if err != nil {
		return nil, err
	}
	return &Result{Columns: o.layout().columnNames(), Rows: rows}, nil
}

func (x *Executor) execMutation(ctx context.Context, plan ir.Node, e *env) (*Result, error) {
	switch n := plan.(type) {
	case *ir.Insert:
		return x.execInsert(ctx, n, e)
	case *ir.Update:
		return x.execUpdate(ctx, n, e)
	case *ir.Delete:
		return x.execDelete(ctx, n, e)
	}
	return nil, fmt.Errorf("exec: unsupported mutation %T", plan)
}

func (x *Executor) batchSize() int {
	if x.BatchSize <= 0 {
		return 256
	}
	return x.BatchSize
}

func (x *Executor) concurrency() int {
	if x.Concurrency <= 0 {
		return 16
	}
	return x.Concurrency
}

// run opens an operator and drains it.
func run(ctx context.Context, o op, e *env) ([]value.Row, error) {
	s, err := o.open(ctx, e)
	if err != nil {
		return nil, err
	}
	defer s.close()
	var out []value.Row
	for {
		b, err := s.next(ctx)
		if err != nil {
			return nil, err
		}
		if b == nil {
			return out, nil
		}
		out = append(out, b...)
	}
}
