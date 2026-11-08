package ledgercore

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDialect_introspection(t *testing.T) {
	db := openTest(t)
	d := db.conn.d
	want := "sqlite"
	if onPostgres() {
		want = "postgres"
	}
	if d.name() != want {
		t.Fatalf("name = %q, want %q", d.name(), want)
	}
	tables, err := d.tables(ctx, db.conn)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"items", "item_labels", "project_members", "audit_log", "workflows"} {
		found := false
		for _, n := range tables {
			if n == want {
				found = true
			}
			if strings.HasPrefix(n, "sqlite_") || strings.HasPrefix(n, "pg_") {
				t.Errorf("the database's own table %q is listed", n)
			}
		}
		if !found {
			t.Errorf("table %q not listed in %v", want, tables)
		}
	}
	cols, err := d.columns(ctx, db.conn, "items")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cols {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names[:3], []string{"id", "project_id", "parent_id"}) {
		t.Errorf("columns are in table order: %v", names)
	}
	pk, err := primaryKey(cols, "items")
	if err != nil || pk != "id" {
		t.Errorf("primary key = %q %v", pk, err)
	}
	if _, err := d.columns(ctx, db.conn, "no_such_table"); err != nil {
		// SQLite reports an unknown table as no columns; either is acceptable as long as it is not a panic.
		t.Logf("unknown table: %v", err)
	}
	// Every table the ledger has names exactly one primary key, which the timestamp
	// rewrite depends on.
	for _, tb := range tables {
		cs, err := d.columns(ctx, db.conn, tb)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := primaryKey(cs, tb); err != nil {
			t.Errorf("%v", err)
		}
	}
}

func TestPrimaryKey_refusesNoneAndComposite(t *testing.T) {
	if _, err := primaryKey([]columnInfo{{"a", false}, {"b", false}}, "t"); err == nil {
		t.Error("a table with no primary key was accepted")
	}
	if _, err := primaryKey([]columnInfo{{"a", true}, {"b", true}}, "t"); err == nil {
		t.Error("a composite primary key was accepted")
	}
}

func TestSQLiteDialect_statements(t *testing.T) {
	skipOnPostgres(t, "asserts the SQLite spellings of the statements")
	d := sqliteDialect{}
	if got := d.insertIgnore("t", "a", "b", "c"); got != "INSERT OR IGNORE INTO t (a, b, c) VALUES (?, ?, ?)" {
		t.Errorf("insertIgnore: %q", got)
	}
	if got := d.notStamp("created_at"); !strings.HasPrefix(got, "created_at NOT GLOB '") {
		t.Errorf("notStamp: %q", got)
	}
	if placeholders(0) != "" || placeholders(1) != "?" || placeholders(3) != "?, ?, ?" {
		t.Errorf("placeholders: %q %q %q", placeholders(0), placeholders(1), placeholders(3))
	}
}

// insertIgnore really ignores a duplicate key, and keeps the first row.
func TestSQLiteDialect_insertIgnoreKeepsTheFirstRow(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "demo")
	stmt := db.conn.d.insertIgnore("project_aliases", "alias_key", "project_id", "created_at")
	for _, stamp := range []string{"2026-01-01T00:00:00.000000000Z", "2026-02-02T00:00:00.000000000Z"} {
		if _, err := db.conn.Exec(stmt, "old-key", p.ID, stamp); err != nil {
			t.Fatalf("insertIgnore: %v", err)
		}
	}
	var n int
	var created string
	db.conn.QueryRow(`SELECT COUNT(*), MIN(created_at) FROM project_aliases WHERE alias_key = 'old-key'`).Scan(&n, &created)
	if n != 1 || created != "2026-01-01T00:00:00.000000000Z" {
		t.Errorf("rows=%d created=%q", n, created)
	}
}

// The two factories each hand out their own kind of ledger, and never the other's.
func TestFactories_eachOpensItsOwnKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	opts := backendOptions(t, path)

	if _, err := (PostgresFactory{Options: Options{Path: path}}).Open(); err == nil {
		t.Error("a PostgreSQL factory with no DSN opened something")
	}
	if onPostgres() {
		s, err := PostgresFactory{Options: opts}.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if s.(*DB).conn.d.name() != "postgres" {
			t.Errorf("PostgresFactory opened %s", s.(*DB).conn.d.name())
		}
		// Given a DSN, the SQLite factory still opens SQLite (here, a file beside the test).
		sq, err := SQLiteFactory{Options: Options{Path: filepath.Join(t.TempDir(), "other.db"), DSN: opts.DSN}}.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer sq.Close()
		if sq.(*DB).conn.d.name() != "sqlite" {
			t.Errorf("SQLiteFactory opened %s", sq.(*DB).conn.d.name())
		}
		return
	}
	s, err := SQLiteFactory{Options: opts}.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.(*DB).conn.d.name() != "sqlite" {
		t.Errorf("SQLiteFactory opened %s", s.(*DB).conn.d.name())
	}
}
