package kvstore

import (
	"bytes"
	"context"
	"fmt"

	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/value"
)

// Apply validates and applies a batch of mutations atomically. Writes are
// serialized by a store-wide mutex, which makes existence, uniqueness and
// optimistic (Old image) checks race-free within one process.
func (s *Store) Apply(ctx context.Context, batch []storage.Mutation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	t := &txn{kv: s.kv, writes: map[string][]byte{}}
	for i := range batch {
		if err := t.apply(&batch[i]); err != nil {
			return err
		}
	}
	return t.commit()
}

// txn overlays pending writes on the KV so that later mutations in a batch
// observe earlier ones. A nil value in writes is a deletion.
type txn struct {
	kv     KV
	writes map[string][]byte
	order  []string
}

func (t *txn) get(k []byte) ([]byte, bool, error) {
	if v, ok := t.writes[string(k)]; ok {
		return v, v != nil, nil
	}
	return t.kv.Get(k)
}

func (t *txn) put(k, v []byte) {
	if v == nil {
		v = []byte{}
	}
	t.record(k, v)
}

func (t *txn) del(k []byte) { t.record(k, nil) }

func (t *txn) record(k, v []byte) {
	if _, ok := t.writes[string(k)]; !ok {
		t.order = append(t.order, string(k))
	}
	t.writes[string(k)] = v
}

func (t *txn) commit() error {
	if len(t.order) == 0 {
		return nil
	}
	b := t.kv.NewBatch()
	for _, k := range t.order {
		if v := t.writes[k]; v != nil {
			b.Set([]byte(k), v)
		} else {
			b.Delete([]byte(k))
		}
	}
	return b.Commit()
}

func (t *txn) apply(m *storage.Mutation) error {
	rel := m.Rel
	switch m.Kind {
	case storage.Insert:
		if err := checkRow(rel, m.New); err != nil {
			return err
		}
		pk := rel.PK(m.New)
		rk, err := rowKey(rel, pk)
		if err != nil {
			return err
		}
		if _, exists, err := t.get(rk); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%w: %s.primary %v", storage.ErrDuplicateKey, rel.Name, pk)
		}
		return t.writeRow(rel, rk, nil, m.New)

	case storage.Update:
		if err := checkRow(rel, m.New); err != nil {
			return err
		}
		pk := rel.PK(m.Old)
		if value.CompareTuples(pk, rel.PK(m.New)) != 0 {
			return fmt.Errorf("kvstore: %s: primary key cannot be updated", rel.Name)
		}
		rk, err := t.checkOld(rel, m.Old)
		if err != nil {
			return err
		}
		return t.writeRow(rel, rk, m.Old, m.New)

	case storage.Delete:
		rk, err := t.checkOld(rel, m.Old)
		if err != nil {
			return err
		}
		t.del(rk)
		for _, p := range rel.Paths {
			ik, err := indexKey(rel, p, m.Old)
			if err != nil {
				return err
			}
			t.del(ik)
		}
		return nil
	}
	return fmt.Errorf("kvstore: unknown mutation kind %d", m.Kind)
}

// checkOld verifies that the stored row equals old and returns its key.
func (t *txn) checkOld(rel *schema.Relation, old value.Row) ([]byte, error) {
	if len(old) != len(rel.Columns) {
		return nil, fmt.Errorf("kvstore: %s: old row has %d columns, want %d", rel.Name, len(old), len(rel.Columns))
	}
	pk := rel.PK(old)
	rk, err := rowKey(rel, pk)
	if err != nil {
		return nil, err
	}
	b, ok, err := t.get(rk)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s %v no longer exists", storage.ErrConflict, rel.Name, pk)
	}
	stored, err := decodeRow(rel, b)
	if err != nil {
		return nil, err
	}
	if !stored.Equal(old) {
		return nil, fmt.Errorf("%w: %s %v was modified", storage.ErrConflict, rel.Name, pk)
	}
	return rk, nil
}

// writeRow writes the row and maintains every secondary index. old is nil
// for inserts.
func (t *txn) writeRow(rel *schema.Relation, rk []byte, old, row value.Row) error {
	enc, err := encodeRow(row)
	if err != nil {
		return err
	}
	pkEnc := rk[8:]
	for _, p := range rel.Paths {
		nk, err := indexKey(rel, p, row)
		if err != nil {
			return err
		}
		if old != nil {
			ok, err := indexKey(rel, p, old)
			if err != nil {
				return err
			}
			if bytes.Equal(ok, nk) {
				continue
			}
			t.del(ok)
		}
		if p.Unique {
			if err := t.checkUnique(rel, p, row, pkEnc); err != nil {
				return err
			}
		}
		t.put(nk, nil)
	}
	t.put(rk, enc)
	return nil
}

// checkUnique fails if another row already holds row's key on unique path p.
// Keys containing NULL never conflict.
func (t *txn) checkUnique(rel *schema.Relation, p *schema.AccessPath, row value.Row, pkEnc []byte) error {
	key := schema.KeyOf(row, p.KeyCols())
	if key.HasNull() {
		return nil
	}
	prefix, err := value.EncodeTuple(pathPrefix(rel, p), key)
	if err != nil {
		return err
	}
	dup := fmt.Errorf("%w: %s.%s %v", storage.ErrDuplicateKey, rel.Name, p.Name, key)
	for k, v := range t.writes {
		if v != nil && bytes.HasPrefix([]byte(k), prefix) && !bytes.Equal([]byte(k)[len(prefix):], pkEnc) {
			return dup
		}
	}
	it, err := t.kv.NewIter(prefix, value.PrefixEnd(prefix), false)
	if err != nil {
		return err
	}
	defer it.Close()
	for it.Next() {
		k := it.Key()
		if _, overlaid := t.writes[string(k)]; overlaid {
			continue // pending deletes hide it; pending sets were checked above
		}
		if !bytes.Equal(k[len(prefix):], pkEnc) {
			return dup
		}
	}
	return it.Err()
}

// checkRow verifies width, value kinds and nullability against the schema.
func checkRow(rel *schema.Relation, row value.Row) error {
	if len(row) != len(rel.Columns) {
		return fmt.Errorf("kvstore: %s: row has %d columns, want %d", rel.Name, len(row), len(rel.Columns))
	}
	for i, c := range rel.Columns {
		v := row[i]
		if v.IsNull() {
			if !c.Nullable {
				return fmt.Errorf("kvstore: %s.%s: NULL in non-nullable column", rel.Name, c.Name)
			}
			continue
		}
		if v.Kind() != c.Type.Kind() {
			return fmt.Errorf("kvstore: %s.%s: %s value in %s column", rel.Name, c.Name, v.Kind(), c.Type)
		}
	}
	return nil
}
