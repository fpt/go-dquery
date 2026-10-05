package pebble_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fpt/go-dquery/internal/fixture"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage"
	"github.com/fpt/go-dquery/storage/pebble"
	"github.com/fpt/go-dquery/storage/storagetest"
	"github.com/fpt/go-dquery/value"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store {
		s, err := pebble.OpenInMemory(pebble.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	cat := fixture.Shop()
	users := cat.Relation("users")

	s, err := pebble.Open(dir, pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindCatalog(cat); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, []storage.Mutation{
		{Kind: storage.Insert, Rel: users, New: fixture.User(1, "ann", "ann@x")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = pebble.Open(dir, pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindCatalog(cat); err != nil {
		t.Fatalf("rebinding the same catalog: %v", err)
	}
	row, ok, err := s.Get(ctx, users, users.Path("by_email"), value.Tuple{value.String("ann@x")}, nil)
	if err != nil || !ok || row[1].Str() != "ann" {
		t.Fatalf("after reopen: %v %v %v", row, ok, err)
	}
}

func TestBindCatalogRejectsLayoutChanges(t *testing.T) {
	dir := t.TempDir()
	s, err := pebble.Open(dir, pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindCatalog(fixture.Shop()); err != nil {
		t.Fatal(err)
	}

	// Adding a relation is allowed.
	withTable := strings.Replace(fixture.ShopYAML, "relationships:", `  - name: notes
    columns:
      - {name: id, type: int}
    primary: {partition: [id]}

relationships:`, 1)
	if err := s.BindCatalog(mustParse(t, withTable)); err != nil {
		t.Fatalf("adding a relation: %v", err)
	}

	for name, src := range map[string]string{
		"column type changed": strings.Replace(fixture.ShopYAML, "{name: qty, type: int}", "{name: qty, type: float}", 1),
		"path changed":        strings.Replace(fixture.ShopYAML, "sort: [created_at]}\n      - {name: by_status", "sort: []}\n      - {name: by_status", 1),
		"relations reordered": reorder(t),
		"relation removed":    strings.Replace(withTable, "  - name: notes\n    columns:\n      - {name: id, type: int}\n    primary: {partition: [id]}\n", "", 1),
	} {
		if err := s.BindCatalog(mustParse(t, src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func mustParse(t *testing.T, src string) *schema.Catalog {
	t.Helper()
	c, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// reorder swaps users and orders so that their positional IDs change.
func reorder(t *testing.T) string {
	src := fixture.ShopYAML
	ui := strings.Index(src, "  - name: users")
	oi := strings.Index(src, "  - name: orders")
	ii := strings.Index(src, "  - name: items")
	if ui < 0 || oi < 0 || ii < 0 {
		t.Fatal("fixture layout changed")
	}
	return src[:ui] + src[oi:ii] + src[ui:oi] + src[ii:]
}
