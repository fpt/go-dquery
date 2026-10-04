package kvstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/value"
)

// Key layout:
//
//	row:   <relID:4><primaryPathID:4><enc(pk)>                     -> enc(row)
//	index: <relID:4><pathID:4><enc(path key cols)><enc(pk)>        -> (empty)
//
// Unique secondary paths use the index layout; uniqueness is enforced by a
// prefix check under the write lock.

// Store implements storage.Store over a KV engine.
type Store struct {
	kv KV
	mu sync.Mutex // serializes Apply
}

var _ storage.Store = (*Store)(nil)

func New(kv KV) *Store { return &Store{kv: kv} }

// Close closes the underlying KV.
func (s *Store) Close() error { return s.kv.Close() }

func (s *Store) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		OrderedScan:    true,
		ReverseScan:    true,
		AtomicMultiRow: true,
	}
}

func pathPrefix(rel *schema.Relation, path *schema.AccessPath) []byte {
	b := make([]byte, 8, 64)
	binary.BigEndian.PutUint32(b, uint32(rel.ID))
	binary.BigEndian.PutUint32(b[4:], uint32(path.ID))
	return b
}

func rowKey(rel *schema.Relation, pk value.Tuple) ([]byte, error) {
	return value.EncodeTuple(pathPrefix(rel, rel.Primary), pk)
}

func indexKey(rel *schema.Relation, path *schema.AccessPath, row value.Row) ([]byte, error) {
	k, err := value.EncodeTuple(pathPrefix(rel, path), schema.KeyOf(row, path.KeyCols()))
	if err != nil {
		return nil, err
	}
	return value.EncodeTuple(k, rel.PK(row))
}

func encodeRow(row value.Row) ([]byte, error) {
	return value.EncodeTuple(nil, value.Tuple(row))
}

func decodeRow(rel *schema.Relation, b []byte) (value.Row, error) {
	t, rest, err := value.DecodeTuple(b, len(rel.Columns))
	if err != nil {
		return nil, fmt.Errorf("kvstore: %s: corrupt row: %w", rel.Name, err)
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("kvstore: %s: corrupt row: %d trailing bytes", rel.Name, len(rest))
	}
	return value.Row(t), nil
}

func (s *Store) getByPK(rel *schema.Relation, pk value.Tuple) (value.Row, bool, error) {
	k, err := rowKey(rel, pk)
	if err != nil {
		return nil, false, err
	}
	b, ok, err := s.kv.Get(k)
	if err != nil || !ok {
		return nil, false, err
	}
	row, err := decodeRow(rel, b)
	return row, err == nil, err
}

func (s *Store) Get(ctx context.Context, rel *schema.Relation, path *schema.AccessPath, key value.Tuple, cols storage.ColSet) (value.Row, bool, error) {
	if !path.Unique || len(key) != len(path.KeyCols()) {
		return nil, false, fmt.Errorf("kvstore: Get %s.%s needs a full key on a unique path", rel.Name, path.Name)
	}
	if key.HasNull() {
		return nil, false, nil
	}
	if path.Primary {
		return s.getByPK(rel, key)
	}
	it, err := s.Scan(ctx, storage.ScanRequest{Rel: rel, Path: path, Eq: key, Limit: 1, Cols: cols})
	if err != nil {
		return nil, false, err
	}
	defer it.Close()
	if it.Next() {
		return it.Row(), true, nil
	}
	return nil, false, it.Err()
}

func (s *Store) GetMany(ctx context.Context, rel *schema.Relation, path *schema.AccessPath, keys []value.Tuple, cols storage.ColSet) ([]value.Row, error) {
	out := make([]value.Row, len(keys))
	for i, k := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, ok, err := s.Get(ctx, rel, path, k, cols)
		if err != nil {
			return nil, err
		}
		if ok {
			out[i] = row
		}
	}
	return out, nil
}

// scanRange computes the [start, end) key range for a scan request, and
// whether the range is known to be empty.
func scanRange(req *storage.ScanRequest) (start, end []byte, empty bool, err error) {
	if req.Eq.HasNull() ||
		(req.Lo != nil && req.Lo.Value.IsNull()) ||
		(req.Hi != nil && req.Hi.Value.IsNull()) {
		return nil, nil, true, nil // comparisons with NULL never match
	}
	p, err := value.EncodeTuple(pathPrefix(req.Rel, req.Path), req.Eq)
	if err != nil {
		return nil, nil, false, err
	}
	switch {
	case req.Lo != nil:
		k, err := value.EncodeKey(append([]byte(nil), p...), req.Lo.Value)
		if err != nil {
			return nil, nil, false, err
		}
		if req.Lo.Inclusive {
			start = k
		} else {
			start = value.PrefixEnd(k)
		}
	case req.Hi != nil:
		// A bounded range excludes NULLs, which sort first.
		start = value.PrefixEnd(append(append([]byte(nil), p...), nullKey...))
	default:
		start = p
	}
	if req.Hi != nil {
		k, err := value.EncodeKey(append([]byte(nil), p...), req.Hi.Value)
		if err != nil {
			return nil, nil, false, err
		}
		if req.Hi.Inclusive {
			end = value.PrefixEnd(k)
		} else {
			end = k
		}
	} else {
		end = value.PrefixEnd(p)
	}
	if end != nil && bytes.Compare(start, end) >= 0 {
		return nil, nil, true, nil
	}
	return start, end, false, nil
}

var nullKey, _ = value.EncodeKey(nil, value.Null)

func (s *Store) Scan(ctx context.Context, req storage.ScanRequest) (storage.RowIterator, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	start, end, empty, err := scanRange(&req)
	if err != nil {
		return nil, err
	}
	if empty {
		return storage.NewSliceIterator(nil), nil
	}
	it, err := s.kv.NewIter(start, end, req.Reverse)
	if err != nil {
		return nil, err
	}
	return &scanIter{ctx: ctx, s: s, req: req, it: it, prefixLen: 8}, nil
}

type scanIter struct {
	ctx       context.Context
	s         *Store
	req       storage.ScanRequest
	it        Iter
	prefixLen int
	row       value.Row
	n         int
	err       error
}

func (si *scanIter) Next() bool {
	if si.err != nil || (si.req.Limit > 0 && si.n >= si.req.Limit) {
		return false
	}
	for si.it.Next() {
		if si.err = si.ctx.Err(); si.err != nil {
			return false
		}
		row, ok, err := si.decode(si.it.Key(), si.it.Value())
		if err != nil {
			si.err = err
			return false
		}
		if !ok {
			continue
		}
		si.row = row
		si.n++
		return true
	}
	si.err = si.it.Err()
	return false
}

func (si *scanIter) decode(k, v []byte) (value.Row, bool, error) {
	rel, path := si.req.Rel, si.req.Path
	if path.Primary {
		row, err := decodeRow(rel, v)
		return row, err == nil, err
	}
	// Index entry: skip the path key columns, the remainder is the PK.
	_, rest, err := value.DecodeTuple(k[si.prefixLen:], len(path.KeyCols()))
	if err != nil {
		return nil, false, fmt.Errorf("kvstore: %s.%s: corrupt index key: %w", rel.Name, path.Name, err)
	}
	pk, err := value.DecodeAll(rest)
	if err != nil {
		return nil, false, err
	}
	row, ok, err := si.s.getByPK(rel, pk)
	if err != nil || !ok {
		return nil, false, err
	}
	// Skip entries made stale by a concurrent write between the index read
	// and the row read.
	cur, err := indexKey(rel, path, row)
	if err != nil {
		return nil, false, err
	}
	return row, bytes.Equal(cur, k), nil
}

func (si *scanIter) Row() value.Row { return si.row }
func (si *scanIter) Err() error     { return si.err }
func (si *scanIter) Close() error   { return si.it.Close() }
