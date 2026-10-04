// Package kvstore implements storage.Store on top of an ordered key-value
// engine. It owns the row and index encoding and the write path with index
// maintenance; KV engines (memory, Pebble, RocksDB) only provide ordered
// get/iterate/batch-write.
package kvstore

// KV is the minimal ordered key-value engine kvstore needs. Implementations
// must be safe for concurrent use. Returned byte slices must not be modified
// by the caller and remain valid until the next call on the same iterator.
type KV interface {
	Get(key []byte) (value []byte, ok bool, err error)
	// NewIter iterates keys in [lo, hi) in ascending order, or descending
	// when reverse is set. A nil hi means unbounded.
	NewIter(lo, hi []byte, reverse bool) (Iter, error)
	NewBatch() Batch
	Close() error
}

type Iter interface {
	Next() bool
	Key() []byte
	Value() []byte
	Err() error
	Close() error
}

// Batch collects writes that Commit applies atomically.
type Batch interface {
	Set(key, value []byte)
	Delete(key []byte)
	Commit() error
}
