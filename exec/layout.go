package exec

import (
	"fmt"

	"github.com/fpt/go-dquery/schema"
)

type field struct{ qual, name string }

// layout names the positions of a row.
type layout struct{ fields []field }

func relLayout(rel *schema.Relation, alias string) *layout {
	l := &layout{fields: make([]field, len(rel.Columns))}
	for i, c := range rel.Columns {
		l.fields[i] = field{qual: alias, name: c.Name}
	}
	return l
}

func concat(a, b *layout) *layout {
	fs := make([]field, 0, len(a.fields)+len(b.fields))
	fs = append(fs, a.fields...)
	return &layout{fields: append(fs, b.fields...)}
}

func (l *layout) width() int { return len(l.fields) }

// resolve finds a column by optional qualifier and name.
func (l *layout) resolve(qual, name string) (int, error) {
	idx := -1
	for i, f := range l.fields {
		if f.name != name || (qual != "" && f.qual != qual) {
			continue
		}
		if idx >= 0 {
			return 0, fmt.Errorf("exec: column reference %q is ambiguous", qualName(qual, name))
		}
		idx = i
	}
	if idx < 0 {
		return 0, fmt.Errorf("exec: unknown column %q", qualName(qual, name))
	}
	return idx, nil
}

// columnNames returns output column names, qualifying names that would
// otherwise be duplicated.
func (l *layout) columnNames() []string {
	count := map[string]int{}
	for _, f := range l.fields {
		count[f.name]++
	}
	out := make([]string, len(l.fields))
	for i, f := range l.fields {
		if count[f.name] > 1 {
			out[i] = qualName(f.qual, f.name)
		} else {
			out[i] = f.name
		}
	}
	return out
}

func qualName(q, n string) string {
	if q == "" {
		return n
	}
	return q + "." + n
}
