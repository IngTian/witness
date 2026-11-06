package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// An UPGRADED archive must end with exactly the schema a FRESH one gets.
//
// testdata/schema/vN.sql is the schema a fresh vN archive had, dumped from that version's own code.
// Each is loaded, opened (which migrates it), and compared to a fresh archive by SHAPE: columns with
// type, notnull, default and pk; foreign keys; AUTOINCREMENT; indexes. Order-insensitive, because an
// ALTER appends a column that a fresh CREATE may list elsewhere — a difference with no meaning.
//
// This class of bug has shipped twice in development: an index in schemaV1 that references a column a
// pre-vN table does not have yet (#171/#174 — the archive could not open), and a column added under
// an already-stamped version, so migrate() early-returned and never added it (#172). The newest
// fixture is the guard for the second: change schemaV1 without bumping schemaVersion and the
// current-version fixture no longer matches. On a bump, add the new version's dump (see dumpEnv).
func TestUpgradedSchemaMatchesFresh(t *testing.T) {
	fresh := tempStore(t)
	if os.Getenv(dumpEnv) == "1" {
		out := filepath.Join("testdata", "schema", fmt.Sprintf("v%d.sql", schemaVersion))
		if err := os.WriteFile(out, []byte(schemaDump(t, fresh.db)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", out)
	}
	want := schemaShape(t, fresh.db)

	files, err := filepath.Glob(filepath.Join("testdata", "schema", "v*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Fatalf("found %d schema fixtures, want at least 5 — the glob is wrong and this test is vacuous", len(files))
	}
	var sawCurrent bool
	for _, f := range files {
		sawCurrent = sawCurrent || filepath.Base(f) == fmt.Sprintf("v%d.sql", schemaVersion)
		t.Run(filepath.Base(f), func(t *testing.T) {
			dump, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			raw, err := sql.Open("sqlite", filepath.Join(home, "witness.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(string(dump)); err != nil {
				raw.Close()
				t.Fatalf("load fixture: %v", err)
			}
			raw.Close()

			t.Setenv("WITNESS_HOME", home)
			s, err := Open()
			if err != nil {
				t.Fatalf("an archive at this version could not be opened: %v", err)
			}
			defer s.Close()
			got := schemaShape(t, s.db)
			for _, l := range want {
				if !slices.Contains(got, l) {
					t.Errorf("upgraded archive lacks: %s", l)
				}
			}
			for _, l := range got {
				if !slices.Contains(want, l) {
					t.Errorf("upgraded archive has, fresh does not: %s", l)
				}
			}
		})
	}
	if !sawCurrent {
		t.Errorf("no testdata/schema/v%d.sql: a schema bump must add the new version's dump "+
			"(%s=1 go test -run TestUpgradedSchemaMatchesFresh ./internal/store/)", schemaVersion, dumpEnv)
	}
}

// dumpEnv makes TestUpgradedSchemaMatchesFresh (re)write testdata/schema/v<schemaVersion>.sql from a
// fresh archive. Run it once when bumping schemaVersion, and commit the file.
const dumpEnv = "WITNESS_DUMP_SCHEMA"

// schemaDump renders db's schema as SQL a fresh database can execute: user_version, then every
// table, then every index, each in name order. SQLite's internal objects are left out (it creates
// sqlite_sequence itself for an AUTOINCREMENT table; autoindexes come from the CREATE TABLE).
func schemaDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY type = 'index', name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	b := &strings.Builder{}
	fmt.Fprintf(b, "PRAGMA user_version=%d;\n", v)
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			t.Fatal(err)
		}
		b.WriteString(stmt + ";\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// schemaShape is db's schema as a sorted set of facts, one per line, free of declaration order.
func schemaShape(t *testing.T, db *sql.DB) []string {
	t.Helper()
	query := func(q string, args ...any) [][]string {
		t.Helper()
		rows, err := db.Query(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var out [][]string
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			row := make([]string, len(cols))
			for i, v := range vals {
				row[i] = v.String
				if !v.Valid {
					row[i] = "NULL"
				}
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	var shape []string
	for _, tbl := range query(`SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`) {
		name := tbl[0]
		shape = append(shape, fmt.Sprintf("table %s autoincrement=%v", name,
			strings.Contains(strings.ToUpper(tbl[1]), "AUTOINCREMENT")))
		for _, c := range query(`SELECT name, upper(type), "notnull", dflt_value, pk FROM pragma_table_info(?)`, name) {
			shape = append(shape, fmt.Sprintf("column %s.%s type=%s notnull=%s default=%s pk=%s", name, c[0], c[1], c[2], c[3], c[4]))
		}
		for _, fk := range query(`SELECT "from", "table", "to", on_update, on_delete FROM pragma_foreign_key_list(?)`, name) {
			shape = append(shape, fmt.Sprintf("fk %s.%s -> %s.%s update=%s delete=%s", name, fk[0], fk[1], fk[2], fk[3], fk[4]))
		}
		for _, ix := range query(`SELECT name, "unique", origin, partial FROM pragma_index_list(?)`, name) {
			var cols []string
			for _, c := range query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, ix[0]) {
				cols = append(cols, c[0])
			}
			// An autoindex's name is a counter over the table's constraints, not a decision anyone
			// made; name only the indexes a CREATE INDEX named.
			label := ix[0]
			if ix[2] != "c" {
				label = "(" + ix[2] + ")"
			}
			shape = append(shape, fmt.Sprintf("index %s %s on (%s) unique=%s partial=%s", name, label,
				strings.Join(cols, ","), ix[1], ix[3]))
		}
	}
	slices.Sort(shape)
	return shape
}
