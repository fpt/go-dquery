// Package memory provides an in-memory ordered KV engine and a Store built on
// it via kvstore.
package memory

import (
	"bytes"
	"sync"

	"github.com/tidwall/btree"

	"github.com/fpt/go-dquery/storage/kvstore"
)

// NewStore returns an empty in-memory store.
func NewStore() *kvstore.Store { return kvstore.New(NewKV()) }

type item struct{ k, v []byte }

func less(a, b item) bool { return bytes.Compare(a.k, b.k) < 0 }

// KV is an ordered in-memory KV. Iterators read from a copy-on-write
// snapshot taken when they are created.
type KV struct {
	mu sync.RWMutex
	tr *btree.BTreeG[item]
}

var _ kvstore.KV = (*KV)(nil)

func NewKV() *KV {
	return &KV{tr: btree.NewBTreeGOptions(less, btree.Options{NoLocks: true})}
}

func (m *KV) Get(key []byte) ([]byte, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	it, ok := m.tr.Get(item{k: key})
	return it.v, ok, nil
}

func (m *KV) NewIter(lo, hi []byte, reverse bool) (kvstore.Iter, error) {
	m.mu.Lock() // Copy mutates copy-on-write bookkeeping of the source tree
	snap := m.tr.Copy()
	m.mu.Unlock()
	return &iter{it: snap.Iter(), lo: lo, hi: hi, reverse: reverse}, nil
}

func (m *KV) NewBatch() kvstore.Batch { return &batch{kv: m} }

func (m *KV) Close() error { return nil }

type iter struct {
	it      btree.IterG[item]
	lo, hi  []byte
	reverse bool
	started bool
	done    bool
}

func (i *iter) Next() bool {
	if i.done {
		return false
	}
	var ok bool
	switch {
	case i.started && !i.reverse:
		ok = i.it.Next()
	case i.started:
		ok = i.it.Prev()
	case !i.reverse:
		ok = i.it.Seek(item{k: i.lo})
	case i.hi == nil:
		ok = i.it.Last()
	default:
		// Seek finds the first key >= hi; step back to the last key < hi.
		if i.it.Seek(item{k: i.hi}) {
			ok = i.it.Prev()
		} else {
			ok = i.it.Last()
		}
	}
	i.started = true
	if ok {
		k := i.it.Item().k
		if i.reverse {
			ok = bytes.Compare(k, i.lo) >= 0
		} else {
			ok = i.hi == nil || bytes.Compare(k, i.hi) < 0
		}
	}
	if !ok {
		i.done = true
		i.it.Release()
	}
	return ok
}

func (i *iter) Key() []byte   { return i.it.Item().k }
func (i *iter) Value() []byte { return i.it.Item().v }
func (i *iter) Err() error    { return nil }
func (i *iter) Close() error {
	if !i.done {
		i.done = true
		i.it.Release()
	}
	return nil
}

type batch struct {
	kv  *KV
	ops []item // v == nil means delete
}

func (b *batch) Set(key, value []byte) {
	b.ops = append(b.ops, item{k: bytes.Clone(key), v: append([]byte{}, value...)})
}

func (b *batch) Delete(key []byte) { b.ops = append(b.ops, item{k: bytes.Clone(key)}) }

func (b *batch) Commit() error {
	b.kv.mu.Lock()
	defer b.kv.mu.Unlock()
	for _, op := range b.ops {
		if op.v == nil {
			b.kv.tr.Delete(op)
		} else {
			b.kv.tr.Set(op)
		}
	}
	b.ops = nil
	return nil
}
