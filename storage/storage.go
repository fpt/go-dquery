// Package storage defines the relation-level interface that storage adapters
// implement, together with the capabilities they advertise to the planner.
package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/value"
)

// Store reads and writes rows of relations through access paths.
//
// Rows exchanged with a Store are always full-width (one value per relation
// column, in column order). Cols is a projection hint: a store may leave
// unrequested columns NULL, but must not reorder them.
type Store interface {
	Capabilities() Capabilities

	// Get returns the row whose key on a unique path equals key.
	Get(ctx context.Context, rel *schema.Relation, path *schema.AccessPath, key value.Tuple, cols ColSet) (value.Row, bool, error)

	// GetMany is Get for many keys. The result is aligned with keys; a
	// missing row is nil.
	GetMany(ctx context.Context, rel *schema.Relation, path *schema.AccessPath, keys []value.Tuple, cols ColSet) ([]value.Row, error)

	// Scan returns rows in path order (or reverse order).
	Scan(ctx context.Context, req ScanRequest) (RowIterator, error)

	// Apply applies all mutations atomically, or none of them.
	Apply(ctx context.Context, batch []Mutation) error
}

// ColSet lists the columns a reader needs. Nil means all columns.
type ColSet []schema.ColID

// Bound is one end of a range on a single key column.
type Bound struct {
	Value     value.Value
	Inclusive bool
}

// ScanRequest describes a scan over an access path. Eq binds a prefix of the
// path's key columns (it must cover the whole partition); Lo and Hi bound the
// key column following that prefix.
//
// A request with no Eq and no bounds is a full scan of the path, which is
// allowed regardless of the partition.
//
// When Lo or Hi is set, rows whose bounded column is NULL are excluded.
type ScanRequest struct {
	Rel     *schema.Relation
	Path    *schema.AccessPath
	Eq      value.Tuple
	Lo, Hi  *Bound
	Reverse bool
	Limit   int // 0 = no limit
	Cols    ColSet
}

// Validate checks the request against the path definition.
func (r *ScanRequest) Validate() error {
	keyCols := r.Path.KeyCols()
	if r.Limit < 0 {
		return fmt.Errorf("storage: scan: negative limit")
	}
	if r.IsFull() {
		return nil
	}
	if len(r.Eq) < len(r.Path.Partition) {
		return fmt.Errorf("storage: scan %s.%s: partition must be bound by equality", r.Rel.Name, r.Path.Name)
	}
	if len(r.Eq) > len(keyCols) {
		return fmt.Errorf("storage: scan %s.%s: %d equality values for %d key columns", r.Rel.Name, r.Path.Name, len(r.Eq), len(keyCols))
	}
	if (r.Lo != nil || r.Hi != nil) && len(r.Eq) == len(keyCols) {
		return fmt.Errorf("storage: scan %s.%s: range bound without a remaining key column", r.Rel.Name, r.Path.Name)
	}
	return nil
}

// IsFull reports whether the request scans the whole path.
func (r *ScanRequest) IsFull() bool { return len(r.Eq) == 0 && r.Lo == nil && r.Hi == nil }

// RowIterator iterates over scan results. Usage:
//
//	for it.Next() { row := it.Row() }
//	if err := it.Err(); err != nil { ... }
//	it.Close()
type RowIterator interface {
	Next() bool
	Row() value.Row
	Err() error
	Close() error
}

type MutationKind uint8

const (
	Insert MutationKind = iota + 1
	Update
	Delete
)

func (k MutationKind) String() string {
	switch k {
	case Insert:
		return "insert"
	case Update:
		return "update"
	case Delete:
		return "delete"
	}
	return "?"
}

// Mutation is a full-image row change. Old is nil for inserts, New is nil for
// deletes. For Update and Delete the store verifies that the stored row still
// equals Old (optimistic concurrency) and fails with ErrConflict otherwise.
// Primary key columns cannot change in an Update.
type Mutation struct {
	Kind MutationKind
	Rel  *schema.Relation
	Old  value.Row
	New  value.Row
}

// Capabilities describes what a store supports natively.
type Capabilities struct {
	MultiGet           bool // GetMany is cheaper than repeated Get
	OrderedScan        bool
	ReverseScan        bool
	ProjectionPushdown bool
	AtomicMultiRow     bool // Apply is atomic across rows and indexes
}

var (
	// ErrDuplicateKey reports a primary or unique key conflict.
	ErrDuplicateKey = errors.New("storage: duplicate key")
	// ErrConflict reports that a row changed between read and Apply.
	ErrConflict = errors.New("storage: write conflict")
	// ErrUnsupported reports a request the store cannot serve.
	ErrUnsupported = errors.New("storage: unsupported")
)

// SliceIterator is a RowIterator over materialized rows.
type SliceIterator struct {
	rows []value.Row
	i    int
}

func NewSliceIterator(rows []value.Row) *SliceIterator { return &SliceIterator{rows: rows, i: -1} }

func (s *SliceIterator) Next() bool     { s.i++; return s.i < len(s.rows) }
func (s *SliceIterator) Row() value.Row { return s.rows[s.i] }
func (s *SliceIterator) Err() error     { return nil }
func (s *SliceIterator) Close() error   { return nil }
