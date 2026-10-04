package ir

import (
	"errors"
	"fmt"

	"github.com/fpt/go-dquery/schema"
)

// Validate checks structural invariants of a plan: access paths belong to
// their relation, keys have valid lengths, and inputs are present. Name
// resolution is done by the executor.
func Validate(n Node) error {
	if n == nil {
		return errors.New("ir: nil node")
	}
	if err := validateNode(n); err != nil {
		return err
	}
	for _, in := range n.Inputs() {
		if err := Validate(in); err != nil {
			return err
		}
	}
	return nil
}

func validateNode(n Node) error {
	switch n := n.(type) {
	case *Get:
		if err := checkPath(n.Rel, n.Path, n.Cols); err != nil {
			return err
		}
		return checkFullUniqueKey("Get", n.Rel, n.Path, len(n.Key))
	case *GetMany:
		if err := checkPath(n.Rel, n.Path, n.Cols); err != nil {
			return err
		}
		for _, k := range n.Keys {
			if err := checkFullUniqueKey("GetMany", n.Rel, n.Path, len(k)); err != nil {
				return err
			}
		}
	case *Scan:
		if err := checkPath(n.Rel, n.Path, n.Cols); err != nil {
			return err
		}
		if n.Limit < 0 {
			return errors.New("ir: Scan: negative limit")
		}
		if len(n.Eq) == 0 && n.Lo == nil && n.Hi == nil {
			return nil // full scan
		}
		return checkPrefix("Scan", n.Rel, n.Path, len(n.Eq), n.Lo != nil || n.Hi != nil)
	case *Lookup:
		if err := checkPath(n.Rel, n.Path, n.Cols); err != nil {
			return err
		}
		if len(n.Key) == 0 {
			return fmt.Errorf("ir: Lookup %s: empty key", n.Rel.Name)
		}
		if !n.Many {
			return checkFullUniqueKey("Lookup", n.Rel, n.Path, len(n.Key))
		}
		return checkPrefix("Lookup", n.Rel, n.Path, len(n.Key), false)
	case *Map:
		seen := map[string]bool{}
		for _, f := range n.Fields {
			if f.Plan == nil || f.Name == "" || seen[f.Name] {
				return fmt.Errorf("ir: Map: invalid or duplicate field %q", f.Name)
			}
			seen[f.Name] = true
		}
	case *Limit:
		if n.N < 0 {
			return errors.New("ir: Limit: negative count")
		}
	case *Insert:
		if (n.Input == nil) == (n.Rows == nil) {
			return errors.New("ir: Insert: exactly one of Rows or Input is required")
		}
		if n.OnConflict != nil && n.OnConflict.DoNothing && n.OnConflict.Set != nil {
			return errors.New("ir: Insert: on-conflict cannot both do nothing and update")
		}
	case *Update, *Delete:
		for _, in := range n.Inputs() {
			if in == nil {
				return fmt.Errorf("ir: %T: missing input", n)
			}
			if in.Shape() == ShapeEffect {
				return fmt.Errorf("ir: %T: input must be a read plan", n)
			}
		}
	}
	for _, in := range n.Inputs() {
		if in == nil {
			return fmt.Errorf("ir: %T: missing input", n)
		}
	}
	return nil
}

func checkPath(rel *schema.Relation, path *schema.AccessPath, cols []schema.ColID) error {
	if rel == nil || path == nil {
		return errors.New("ir: missing relation or access path")
	}
	if rel.PathByID(path.ID) != path {
		return fmt.Errorf("ir: access path %q does not belong to %s", path.Name, rel.Name)
	}
	for _, c := range cols {
		if int(c) < 0 || int(c) >= len(rel.Columns) {
			return fmt.Errorf("ir: %s: column id %d out of range", rel.Name, c)
		}
	}
	return nil
}

func checkFullUniqueKey(op string, rel *schema.Relation, path *schema.AccessPath, n int) error {
	if !path.Unique {
		return fmt.Errorf("ir: %s %s.%s: path is not unique", op, rel.Name, path.Name)
	}
	if want := len(path.KeyCols()); n != want {
		return fmt.Errorf("ir: %s %s.%s: key has %d values, want %d", op, rel.Name, path.Name, n, want)
	}
	return nil
}

func checkPrefix(op string, rel *schema.Relation, path *schema.AccessPath, n int, ranged bool) error {
	keyCols := len(path.KeyCols())
	if n < len(path.Partition) {
		return fmt.Errorf("ir: %s %s.%s: partition must be bound by equality", op, rel.Name, path.Name)
	}
	if n > keyCols || (ranged && n == keyCols) {
		return fmt.Errorf("ir: %s %s.%s: %d key values do not fit %d key columns", op, rel.Name, path.Name, n, keyCols)
	}
	return nil
}
