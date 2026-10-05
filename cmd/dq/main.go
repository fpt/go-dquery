// Command dq is an interactive SQL shell over an in-memory go-dquery store.
//
//	dq -schema shop.yaml [-allow-full-scan] [-c "SQL"] [script.sql ...]
//
// Scripts are executed first; then statements are read from stdin until EOF
// or \q, unless -c is given.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fpt/go-dquery"
	"github.com/fpt/go-dquery/exec"
	"github.com/fpt/go-dquery/frontend/sql"
	"github.com/fpt/go-dquery/schema"
	"github.com/fpt/go-dquery/storage/memory"
	"github.com/fpt/go-dquery/value"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dq", flag.ContinueOnError)
	fs.SetOutput(stderr)
	schemaPath := fs.String("schema", "", "schema file (YAML or JSON); required")
	allowFull := fs.Bool("allow-full-scan", false, "allow unbounded full scans")
	command := fs.String("c", "", "execute this SQL and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *schemaPath == "" {
		fmt.Fprintln(stderr, "dq: -schema is required")
		fs.Usage()
		return 2
	}
	cat, err := schema.Load(*schemaPath)
	if err != nil {
		fmt.Fprintln(stderr, "dq:", err)
		return 1
	}
	sh := &shell{db: dquery.Open(cat, memory.NewStore()), out: stdout, errOut: stderr}
	sh.db.Options.AllowFullScan = *allowFull

	for _, path := range fs.Args() {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(stderr, "dq:", err)
			return 1
		}
		if !sh.execAll(string(src)) {
			return 1
		}
	}
	if *command != "" {
		if !sh.execAll(*command) {
			return 1
		}
		return 0
	}
	sh.interactive = isTerminal(stdin)
	sh.repl(stdin)
	return 0
}

type shell struct {
	db          *dquery.DB
	out, errOut io.Writer
	interactive bool
}

func (sh *shell) repl(in io.Reader) {
	sc := bufio.NewScanner(in)
	var buf strings.Builder
	for {
		if sh.interactive {
			if buf.Len() == 0 {
				fmt.Fprint(sh.out, "dq> ")
			} else {
				fmt.Fprint(sh.out, "... ")
			}
		}
		if !sc.Scan() {
			if strings.TrimSpace(buf.String()) != "" {
				sh.execAll(buf.String())
			}
			return
		}
		line := sc.Text()
		if buf.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), `\`) {
			if !sh.meta(strings.Fields(strings.TrimSpace(line))) {
				return
			}
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		if sql.Complete(buf.String()) {
			sh.execAll(buf.String())
			buf.Reset()
		}
	}
}

// execAll runs every statement in src, printing results and errors. It
// reports whether all statements succeeded.
func (sh *shell) execAll(src string) bool {
	stmts, err := sql.ParseAll(src)
	if err != nil {
		fmt.Fprintln(sh.errOut, "ERROR:", err)
		return false
	}
	ok := true
	for _, s := range stmts {
		res, err := sh.db.ExecStmt(context.Background(), s)
		if err != nil {
			fmt.Fprintln(sh.errOut, "ERROR:", err)
			ok = false
			continue
		}
		printResult(sh.out, s, res)
	}
	return ok
}

// meta handles backslash commands; it returns false to quit.
func (sh *shell) meta(f []string) bool {
	switch f[0] {
	case `\q`:
		return false
	case `\d`:
		if len(f) > 1 {
			sh.describe(f[1])
		} else {
			sh.listTables()
		}
	case `\?`, `\h`:
		fmt.Fprintln(sh.out, `Statements end with ";". Commands:
  \d          list tables
  \d TABLE    describe a table and its access paths
  \q          quit`)
	default:
		fmt.Fprintf(sh.errOut, "ERROR: unknown command %s (try \\?)\n", f[0])
	}
	return true
}

func (sh *shell) listTables() {
	var names []string
	for _, r := range sh.db.Catalog.Relations() {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	rows := make([][]string, len(names))
	for i, n := range names {
		rows[i] = []string{n}
	}
	writeTable(sh.out, []string{"table"}, rows)
}

func (sh *shell) describe(name string) {
	rel := sh.db.Catalog.Relation(name)
	if rel == nil {
		fmt.Fprintf(sh.errOut, "ERROR: unknown table %q\n", name)
		return
	}
	var rows [][]string
	for _, c := range rel.Columns {
		null := "not null"
		if c.Nullable {
			null = "null"
		}
		rows = append(rows, []string{c.Name, c.Type.String(), null})
	}
	writeTable(sh.out, []string{"column", "type", "nullable"}, rows)
	rows = nil
	colNames := func(ids []schema.ColID) string {
		var ns []string
		for _, id := range ids {
			ns = append(ns, rel.Columns[id].Name)
		}
		return strings.Join(ns, ", ")
	}
	for _, p := range rel.AllPaths() {
		unique := ""
		if p.Unique {
			unique = "unique"
		}
		rows = append(rows, []string{p.Name, colNames(p.Partition), colNames(p.Sort), unique})
	}
	writeTable(sh.out, []string{"access path", "partition", "sort", ""}, rows)
	var rels []string
	for _, r := range sh.db.Catalog.Relationships(rel) {
		rels = append(rels, fmt.Sprintf("%s -> %s via %s (%s)", r.Name, r.To.Name, r.Path.Name, r.Kind))
	}
	sort.Strings(rels)
	for _, r := range rels {
		fmt.Fprintln(sh.out, "relationship", r)
	}
}

func printResult(w io.Writer, stmt sql.Statement, res *exec.Result) {
	if _, ok := stmt.(*sql.Explain); ok {
		for _, r := range res.Rows {
			fmt.Fprintln(w, r[0].Str())
		}
		return
	}
	if res.Columns != nil {
		rows := make([][]string, len(res.Rows))
		for i, r := range res.Rows {
			rows[i] = make([]string, len(r))
			for j, v := range r {
				rows[i][j] = display(v)
			}
		}
		writeTable(w, res.Columns, rows)
	}
	switch stmt.(type) {
	case *sql.Select:
		fmt.Fprintf(w, "(%d %s)\n", len(res.Rows), plural(len(res.Rows), "row"))
	default:
		fmt.Fprintf(w, "OK, %d %s affected\n", res.RowsAffected, plural(int(res.RowsAffected), "row"))
	}
}

func display(v value.Value) string {
	if v.Kind() == value.KindString {
		return v.Str()
	}
	return v.String()
}

func plural(n int, s string) string {
	if n == 1 {
		return s
	}
	return s + "s"
}

func writeTable(w io.Writer, header []string, rows [][]string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	line := func(cells []string) {
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = c + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c))
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, " | "), " "))
	}
	line(header)
	seps := make([]string, len(widths))
	for i, n := range widths {
		seps[i] = strings.Repeat("-", n)
	}
	fmt.Fprintln(w, strings.Join(seps, "-+-"))
	for _, r := range rows {
		line(r)
	}
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
