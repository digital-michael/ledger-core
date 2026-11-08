package ledgercore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// The PostgreSQL backend writes its SQL once, in the form the rest of this package
// uses ("?" placeholders, SQLite spellings), and a driver wrapper translates it on
// the way to the server. That keeps the roughly 150 call sites unchanged; these
// tests are the contract for the translation.

func TestRebind_numbersPlaceholdersInOrder(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT 1", "SELECT 1"},
		{"SELECT * FROM t WHERE a = ?", "SELECT * FROM t WHERE a = $1"},
		{"INSERT INTO t (a, b, c) VALUES (?, ?, ?)", "INSERT INTO t (a, b, c) VALUES ($1, $2, $3)"},
		{"UPDATE t SET a = ?, b = ? WHERE id = ? AND x IN (?, ?)", "UPDATE t SET a = $1, b = $2 WHERE id = $3 AND x IN ($4, $5)"},
		{"SELECT ?||'-'||?", "SELECT $1||'-'||$2"},
	}
	for _, c := range cases {
		if got := rebind(c.in); got != c.want {
			t.Errorf("rebind(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRebind_leavesQuotedTextAndCommentsAlone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT '?' , ?", "SELECT '?' , $1"},
		{"SELECT 'it''s ?', ?", "SELECT 'it''s ?', $1"},
		{`SELECT "col?" , ?`, `SELECT "col?" , $1`},
		{"SELECT ? -- is this ? a placeholder?\n, ?", "SELECT $1 -- is this ? a placeholder?\n, $2"},
		{"SELECT /* what? */ ?", "SELECT /* what? */ $1"},
		{"WHERE title LIKE ? ESCAPE '\\'", "WHERE title LIKE $1 ESCAPE '\\'"},
		{"SELECT name FROM sqlite_master WHERE name LIKE 'ledger\\_vocab\\_%' ESCAPE '\\'", "SELECT name FROM sqlite_master WHERE name LIKE 'ledger\\_vocab\\_%' ESCAPE '\\'"},
	}
	for _, c := range cases {
		if got := rebind(c.in); got != c.want {
			t.Errorf("rebind(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRebind_isIdempotentOnAlreadyNumberedSQL(t *testing.T) {
	q := "SELECT * FROM t WHERE a = $1 AND b = $2"
	if got := rebind(q); got != q {
		t.Errorf("rebind changed numbered SQL: %q", got)
	}
}

func TestPostgresSQL_spellsTheSQLiteIdioms(t *testing.T) {
	in := "SELECT (SELECT group_concat(label, char(31)) FROM item_labels WHERE item_id = items.id AND deleted_at IS NULL) FROM items WHERE id = ?"
	want := "SELECT (SELECT string_agg(label, chr(31)) FROM item_labels WHERE item_id = items.id AND deleted_at IS NULL) FROM items WHERE id = $1"
	if got := postgresSQL(in); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	// Inside a string literal nothing is rewritten.
	lit := "SELECT 'group_concat(x, char(31))', ?"
	if got := postgresSQL(lit); got != "SELECT 'group_concat(x, char(31))', $1" {
		t.Errorf("a literal was rewritten: %q", got)
	}
	// Names that merely contain the words are left alone.
	if got := postgresSQL("SELECT my_group_concat(x)"); got != "SELECT my_group_concat(x)" {
		t.Errorf("a longer name was rewritten: %q", got)
	}
}

// Every query the real itemColumns builds survives translation with the right
// number of parameters and no stray SQLite spelling.
func TestPostgresSQL_itemColumnsHaveNoSQLiteSpelling(t *testing.T) {
	got := postgresSQL("SELECT " + itemColumns + " FROM items WHERE id = ?")
	for _, bad := range []string{"group_concat", "char(31)", "?"} {
		if strings.Contains(got, bad) {
			t.Errorf("translated item query still contains %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "string_agg(label, chr(31))") || !strings.HasSuffix(got, "WHERE id = $1") {
		t.Errorf("unexpected translation:\n%s", got)
	}
}

// ---- the driver wrapper ----

// recorder is a minimal database/sql driver that remembers the SQL it was given.
type recorder struct {
	mu      sync.Mutex
	queries []string
	execer  bool // implement ExecerContext/QueryerContext, or force the Prepare path
}

func (r *recorder) log(q string) { r.mu.Lock(); r.queries = append(r.queries, q); r.mu.Unlock() }
func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

type recConnector struct{ r *recorder }

func (c recConnector) Connect(context.Context) (driver.Conn, error) { return &recConn{c.r}, nil }
func (c recConnector) Driver() driver.Driver                        { return nil }

type recConn struct{ r *recorder }

func (c *recConn) Prepare(q string) (driver.Stmt, error) {
	c.r.log("prepare: " + q)
	return &recStmt{}, nil
}
func (c *recConn) Close() error              { return nil }
func (c *recConn) Begin() (driver.Tx, error) { c.r.log("begin"); return recTx{}, nil }

// execConn adds the optional fast-path interfaces.
type execConn struct{ *recConn }

func (c execConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.r.log("exec: " + q)
	return driver.RowsAffected(1), nil
}
func (c execConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.r.log("query: " + q)
	return recRows{}, nil
}

type execConnector struct{ r *recorder }

func (c execConnector) Connect(context.Context) (driver.Conn, error) {
	return execConn{&recConn{c.r}}, nil
}
func (c execConnector) Driver() driver.Driver { return nil }

type recStmt struct{}

func (recStmt) Close() error                               { return nil }
func (recStmt) NumInput() int                              { return -1 }
func (recStmt) Exec([]driver.Value) (driver.Result, error) { return driver.RowsAffected(1), nil }
func (recStmt) Query([]driver.Value) (driver.Rows, error)  { return recRows{}, nil }

type recTx struct{}

func (recTx) Commit() error   { return nil }
func (recTx) Rollback() error { return nil }

type recRows struct{}

func (recRows) Columns() []string         { return []string{"x"} }
func (recRows) Close() error              { return nil }
func (recRows) Next([]driver.Value) error { return io.EOF }

func TestRebindDriver_translatesEveryPath(t *testing.T) {
	for name, mk := range map[string]func(*recorder) driver.Connector{
		"driver with ExecerContext/QueryerContext": func(r *recorder) driver.Connector { return execConnector{r} },
		"driver that only prepares":                func(r *recorder) driver.Connector { return recConnector{r} },
	} {
		r := &recorder{}
		db := sql.OpenDB(newTranslatingConnector(mk(r), postgresSQL))
		ctx := context.Background()
		if _, err := db.ExecContext(ctx, "UPDATE t SET a = ? WHERE b = ?", 1, 2); err != nil {
			t.Fatalf("%s: exec: %v", name, err)
		}
		rows, err := db.QueryContext(ctx, "SELECT group_concat(a, char(31)) FROM t WHERE b = ?", 2)
		if err != nil {
			t.Fatalf("%s: query: %v", name, err)
		}
		rows.Close()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("%s: begin: %v", name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (a) VALUES (?)", 3); err != nil {
			t.Fatalf("%s: tx exec: %v", name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		stmt, err := db.PrepareContext(ctx, "DELETE FROM t WHERE a = ?")
		if err != nil {
			t.Fatalf("%s: prepare: %v", name, err)
		}
		stmt.Close()

		all := strings.Join(r.seen(), "\n")
		for _, want := range []string{"UPDATE t SET a = $1 WHERE b = $2", "string_agg(a, chr(31)) FROM t WHERE b = $1", "INSERT INTO t (a) VALUES ($1)", "DELETE FROM t WHERE a = $1"} {
			if !strings.Contains(all, want) {
				t.Errorf("%s: never saw %q in:\n%s", name, want, all)
			}
		}
		if strings.Contains(all, "?") || strings.Contains(all, "group_concat") {
			t.Errorf("%s: untranslated SQL reached the driver:\n%s", name, all)
		}
		db.Close()
	}
}

func TestRebindDriver_keepsTheInnerDriver(t *testing.T) {
	c := newTranslatingConnector(recConnector{&recorder{}}, postgresSQL)
	if !reflect.DeepEqual(c.Driver(), recConnector{}.Driver()) {
		t.Error("Driver() must be the inner driver's")
	}
}
