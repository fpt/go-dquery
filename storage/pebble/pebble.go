// Package pebble provides a persistent Store backed by Pebble, the Go LSM
// key-value engine, via kvstore.
package pebble

import (
	"bytes"
	"errors"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"

	"github.com/fpt/go-dquery/storage/kvstore"
)

// Options configures a Pebble-backed store.
type Options struct {
	// NoSync skips fsync on commit: faster, but the most recent commits may
	// be lost on a machine crash (not on a process crash).
	NoSync bool
	// Pebble, if set, is passed to pebble.Open. Its FS is overridden by
	// OpenInMemory.
	Pebble *pebble.Options
}

// Open opens (or creates) a store in dir.
func Open(dir string, o Options) (*kvstore.Store, error) {
	kv, err := OpenKV(dir, o)
	if err != nil {
		return nil, err
	}
	return kvstore.New(kv), nil
}

// OpenInMemory opens a store on an in-memory filesystem; useful for tests.
func OpenInMemory(o Options) (*kvstore.Store, error) {
	po := &pebble.Options{}
	if o.Pebble != nil {
		po = o.Pebble.Clone()
	}
	po.FS = vfs.NewMem()
	o.Pebble = po
	return Open("", o)
}

// KV adapts a Pebble DB to kvstore.KV.
type KV struct {
	db    *pebble.DB
	write *pebble.WriteOptions
}

var _ kvstore.KV = (*KV)(nil)

// OpenKV opens the raw KV engine.
func OpenKV(dir string, o Options) (*KV, error) {
	po := &pebble.Options{}
	if o.Pebble != nil {
		po = o.Pebble.Clone() // do not modify the caller's options
	}
	if po.Logger == nil && po.LoggerAndTracer == nil {
		po.Logger = quietLogger{}
	}
	db, err := pebble.Open(dir, po)
	if err != nil {
		return nil, err
	}
	w := pebble.Sync
	if o.NoSync {
		w = pebble.NoSync
	}
	return &KV{db: db, write: w}, nil
}

func (k *KV) Get(key []byte) ([]byte, bool, error) {
	v, closer, err := k.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := bytes.Clone(v)
	return out, true, closer.Close()
}

func (k *KV) NewIter(lo, hi []byte, reverse bool) (kvstore.Iter, error) {
	it, err := k.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, err
	}
	return &iter{it: it, reverse: reverse}, nil
}

func (k *KV) NewBatch() kvstore.Batch { return &batch{b: k.db.NewBatch(), write: k.write} }

func (k *KV) Close() error { return k.db.Close() }

// iter reads from Pebble's point-in-time view taken at creation.
type iter struct {
	it      *pebble.Iterator
	reverse bool
	started bool
	val     []byte
	err     error
}

func (i *iter) Next() bool {
	if i.err != nil {
		return false
	}
	var ok bool
	switch {
	case !i.started && i.reverse:
		ok = i.it.Last()
	case !i.started:
		ok = i.it.First()
	case i.reverse:
		ok = i.it.Prev()
	default:
		ok = i.it.Next()
	}
	i.started = true
	if !ok {
		i.err = i.it.Error()
		return false
	}
	i.val, i.err = i.it.ValueAndErr()
	return i.err == nil
}

func (i *iter) Key() []byte   { return i.it.Key() }
func (i *iter) Value() []byte { return i.val }
func (i *iter) Err() error    { return i.err }
func (i *iter) Close() error  { return i.it.Close() }

type batch struct {
	b     *pebble.Batch
	write *pebble.WriteOptions
	err   error
}

func (b *batch) Set(key, value []byte) {
	if b.err == nil {
		b.err = b.b.Set(key, value, nil)
	}
}

func (b *batch) Delete(key []byte) {
	if b.err == nil {
		b.err = b.b.Delete(key, nil)
	}
}

func (b *batch) Commit() error {
	defer b.b.Close()
	if b.err != nil {
		return b.err
	}
	return b.b.Commit(b.write)
}

// quietLogger drops Pebble's informational logs; errors and fatal errors go
// to Pebble's default logger.
type quietLogger struct{}

func (quietLogger) Infof(string, ...any) {}
func (quietLogger) Errorf(format string, args ...any) {
	pebble.DefaultLogger.Errorf(format, args...)
}
func (quietLogger) Fatalf(format string, args ...any) {
	pebble.DefaultLogger.Fatalf(format, args...)
}
