package kvstore

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/fpt/go-dquery/schema"
)

// Relation ID 0 is never assigned by the schema package, so keys starting
// with four zero bytes are free for store metadata.
var catalogKey = append(make([]byte, 8), "catalog"...)

// relSig is the persisted shape of a relation: everything the row and index
// encoding depends on.
type relSig struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Paths   []string `json:"paths"`
}

func (a relSig) equal(b relSig) bool {
	return a.Name == b.Name && slices.Equal(a.Columns, b.Columns) && slices.Equal(a.Paths, b.Paths)
}

func signature(cat *schema.Catalog) map[string]relSig {
	out := map[string]relSig{}
	for _, rel := range cat.Relations() {
		sig := relSig{Name: rel.Name}
		for _, c := range rel.Columns {
			null := "notnull"
			if c.Nullable {
				null = "null"
			}
			sig.Columns = append(sig.Columns, c.Name+":"+c.Type.String()+":"+null)
		}
		names := func(ids []schema.ColID) string {
			var ns []string
			for _, id := range ids {
				ns = append(ns, rel.Columns[id].Name)
			}
			return strings.Join(ns, ",")
		}
		for _, p := range rel.AllPaths() {
			sig.Paths = append(sig.Paths, fmt.Sprintf("%d:%s:partition=%s:sort=%s:unique=%t",
				p.ID, p.Name, names(p.Partition), names(p.Sort), p.Unique))
		}
		out[strconv.FormatUint(uint64(rel.ID), 10)] = sig
	}
	return out
}

// BindCatalog records the catalog's layout in the store, or verifies it
// against the layout recorded earlier. Relations present in the store must
// be unchanged (same ID, columns and access paths); new relations may be
// added. Persistent stores should call it once after opening, before any
// reads or writes.
func (s *Store) BindCatalog(cat *schema.Catalog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := signature(cat)
	b, ok, err := s.kv.Get(catalogKey)
	if err != nil {
		return err
	}
	if ok {
		var stored map[string]relSig
		if err := json.Unmarshal(b, &stored); err != nil {
			return fmt.Errorf("kvstore: corrupt catalog metadata: %w", err)
		}
		ids := make([]string, 0, len(stored))
		for id := range stored {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			old := stored[id]
			now, found := cur[id]
			if !found {
				return fmt.Errorf("kvstore: relation %s (id %s) exists in the store but not in the schema", old.Name, id)
			}
			if !now.equal(old) {
				return fmt.Errorf("kvstore: relation %s (id %s) does not match the stored layout; schema changes are not supported\n  stored: %v\n  schema: %v",
					old.Name, id, old, now)
			}
		}
	}
	enc, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	batch := s.kv.NewBatch()
	batch.Set(catalogKey, enc)
	return batch.Commit()
}
