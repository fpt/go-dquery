package schema

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// CatalogDef is the declarative form of a catalog, used by both the Go API
// and the JSON/YAML loader.
type CatalogDef struct {
	Relations     []RelationDef     `yaml:"relations" json:"relations"`
	Relationships []RelationshipDef `yaml:"relationships" json:"relationships"`
}

type RelationDef struct {
	ID      uint32      `yaml:"id,omitempty" json:"id,omitempty"` // 0 = assigned by position
	Name    string      `yaml:"name" json:"name"`
	Columns []ColumnDef `yaml:"columns" json:"columns"`
	Primary PathDef     `yaml:"primary" json:"primary"`
	Paths   []PathDef   `yaml:"paths" json:"paths"`
}

type ColumnDef struct {
	Name     string `yaml:"name" json:"name"`
	Type     string `yaml:"type" json:"type"`
	Nullable bool   `yaml:"nullable,omitempty" json:"nullable,omitempty"`
}

type PathDef struct {
	ID        uint32   `yaml:"id,omitempty" json:"id,omitempty"` // 0 = assigned by position
	Name      string   `yaml:"name" json:"name"`
	Partition []string `yaml:"partition" json:"partition"`
	Sort      []string `yaml:"sort" json:"sort"`
	Unique    bool     `yaml:"unique,omitempty" json:"unique,omitempty"`
}

type RelationshipDef struct {
	Name     string   `yaml:"name" json:"name"`
	From     string   `yaml:"from" json:"from"`
	FromCols []string `yaml:"from_cols" json:"from_cols"`
	To       string   `yaml:"to" json:"to"`
	Path     string   `yaml:"path" json:"path"` // target access path; default "primary"
	Kind     string   `yaml:"kind" json:"kind"` // one_to_one | one_to_many | many_to_one
}

// Load reads a catalog definition from a YAML or JSON file.
func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses a YAML (or JSON) catalog definition.
func Parse(data []byte) (*Catalog, error) {
	var def CatalogDef
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("schema: parse: %w", err)
	}
	return Build(def)
}

// Build validates def and constructs a Catalog.
func Build(def CatalogDef) (*Catalog, error) {
	c := &Catalog{
		byName:        map[string]*Relation{},
		byID:          map[RelID]*Relation{},
		relationships: map[*Relation]map[string]*Relationship{},
	}
	for i, rd := range def.Relations {
		id := RelID(rd.ID)
		if id == 0 {
			id = RelID(i + 1)
		}
		rel, err := buildRelation(id, rd)
		if err != nil {
			return nil, err
		}
		if c.byName[rel.Name] != nil {
			return nil, fmt.Errorf("schema: duplicate relation %q", rel.Name)
		}
		if c.byID[rel.ID] != nil {
			return nil, fmt.Errorf("schema: duplicate relation id %d", rel.ID)
		}
		c.relations = append(c.relations, rel)
		c.byName[rel.Name] = rel
		c.byID[rel.ID] = rel
	}
	for _, d := range def.Relationships {
		rs, err := c.buildRelationship(d)
		if err != nil {
			return nil, err
		}
		m := c.relationships[rs.From]
		if m == nil {
			m = map[string]*Relationship{}
			c.relationships[rs.From] = m
		}
		if m[rs.Name] != nil || rs.From.Column(rs.Name) != nil {
			return nil, fmt.Errorf("schema: relationship %s.%s conflicts with an existing name", rs.From.Name, rs.Name)
		}
		m[rs.Name] = rs
	}
	return c, nil
}

// MustBuild is Build that panics on error; intended for tests and fixtures.
func MustBuild(def CatalogDef) *Catalog {
	c, err := Build(def)
	if err != nil {
		panic(err)
	}
	return c
}

func buildRelation(id RelID, rd RelationDef) (*Relation, error) {
	if rd.Name == "" {
		return nil, errors.New("schema: relation without name")
	}
	rel := &Relation{
		ID:         id,
		Name:       rd.Name,
		colByName:  map[string]*Column{},
		pathByName: map[string]*AccessPath{},
	}
	for i, cd := range rd.Columns {
		t, err := ParseType(cd.Type)
		if err != nil {
			return nil, fmt.Errorf("schema: %s.%s: %w", rd.Name, cd.Name, err)
		}
		if cd.Name == "" || rel.colByName[cd.Name] != nil {
			return nil, fmt.Errorf("schema: %s: missing or duplicate column name %q", rd.Name, cd.Name)
		}
		col := &Column{ID: ColID(i), Name: cd.Name, Type: t, Nullable: cd.Nullable}
		rel.Columns = append(rel.Columns, col)
		rel.colByName[col.Name] = col
	}

	pd := rd.Primary
	if pd.Name == "" {
		pd.Name = "primary"
	}
	if pd.Name != "primary" {
		return nil, fmt.Errorf("schema: %s: primary path must be named \"primary\"", rd.Name)
	}
	pd.Unique = true
	if pd.ID == 0 {
		pd.ID = uint32(PrimaryPathID)
	}
	if PathID(pd.ID) != PrimaryPathID {
		return nil, fmt.Errorf("schema: %s: primary path id must be %d", rd.Name, PrimaryPathID)
	}
	prim, err := rel.buildPath(pd)
	if err != nil {
		return nil, err
	}
	prim.Primary = true
	if len(prim.KeyCols()) == 0 {
		return nil, fmt.Errorf("schema: %s: primary key has no columns", rd.Name)
	}
	for _, c := range prim.KeyCols() {
		if rel.Columns[c].Nullable {
			return nil, fmt.Errorf("schema: %s: primary key column %q must not be nullable", rd.Name, rel.Columns[c].Name)
		}
	}
	rel.Primary = prim

	for i, pd := range rd.Paths {
		if pd.ID == 0 {
			pd.ID = uint32(PrimaryPathID) + 1 + uint32(i)
		}
		p, err := rel.buildPath(pd)
		if err != nil {
			return nil, err
		}
		rel.Paths = append(rel.Paths, p)
	}
	return rel, nil
}

func (rel *Relation) buildPath(pd PathDef) (*AccessPath, error) {
	if pd.Name == "" {
		return nil, fmt.Errorf("schema: %s: access path without name", rel.Name)
	}
	if rel.pathByName[pd.Name] != nil {
		return nil, fmt.Errorf("schema: %s: duplicate access path %q", rel.Name, pd.Name)
	}
	for _, p := range rel.pathByName {
		if p.ID == PathID(pd.ID) {
			return nil, fmt.Errorf("schema: %s: duplicate access path id %d", rel.Name, pd.ID)
		}
	}
	p := &AccessPath{ID: PathID(pd.ID), Name: pd.Name, Unique: pd.Unique}
	seen := map[string]bool{}
	resolve := func(names []string) ([]ColID, error) {
		var ids []ColID
		for _, n := range names {
			c := rel.colByName[n]
			if c == nil {
				return nil, fmt.Errorf("schema: %s.%s: unknown column %q", rel.Name, pd.Name, n)
			}
			if seen[n] {
				return nil, fmt.Errorf("schema: %s.%s: column %q listed twice", rel.Name, pd.Name, n)
			}
			seen[n] = true
			ids = append(ids, c.ID)
		}
		return ids, nil
	}
	var err error
	if p.Partition, err = resolve(pd.Partition); err != nil {
		return nil, err
	}
	if p.Sort, err = resolve(pd.Sort); err != nil {
		return nil, err
	}
	if len(p.KeyCols()) == 0 {
		return nil, fmt.Errorf("schema: %s.%s: access path has no columns", rel.Name, pd.Name)
	}
	rel.pathByName[p.Name] = p
	return p, nil
}

func (c *Catalog) buildRelationship(d RelationshipDef) (*Relationship, error) {
	from, to := c.byName[d.From], c.byName[d.To]
	if from == nil || to == nil {
		return nil, fmt.Errorf("schema: relationship %q: unknown relation %q or %q", d.Name, d.From, d.To)
	}
	pathName := d.Path
	if pathName == "" {
		pathName = "primary"
	}
	path := to.Path(pathName)
	if path == nil {
		return nil, fmt.Errorf("schema: relationship %q: unknown path %s.%s", d.Name, d.To, pathName)
	}
	var kind RelationshipKind
	for k, n := range relKindNames {
		if n == d.Kind {
			kind = k
		}
	}
	if kind == 0 {
		return nil, fmt.Errorf("schema: relationship %q: unknown kind %q", d.Name, d.Kind)
	}
	keyCols := path.KeyCols()
	if len(d.FromCols) == 0 || len(d.FromCols) > len(keyCols) {
		return nil, fmt.Errorf("schema: relationship %q: from_cols must bind 1..%d key columns of %s.%s", d.Name, len(keyCols), d.To, pathName)
	}
	if len(d.FromCols) < len(path.Partition) {
		return nil, fmt.Errorf("schema: relationship %q: from_cols must bind the whole partition of %s.%s", d.Name, d.To, pathName)
	}
	if kind != OneToMany && !(path.Unique && len(d.FromCols) == len(keyCols)) {
		return nil, fmt.Errorf("schema: relationship %q: %s requires binding a full unique path", d.Name, d.Kind)
	}
	rs := &Relationship{Name: d.Name, From: from, To: to, Path: path, Kind: kind}
	for _, n := range d.FromCols {
		col := from.Column(n)
		if col == nil {
			return nil, fmt.Errorf("schema: relationship %q: unknown column %s.%s", d.Name, d.From, n)
		}
		rs.FromCols = append(rs.FromCols, col.ID)
	}
	return rs, nil
}
