package ledgercore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Backend selection for the contract tests. By default every test runs against a
// SQLite file. With LEDGERCORE_TEST_PG set to a postgres:// URL (make test-pg starts a
// throwaway server in Podman) the very same tests run against PostgreSQL, so the two
// backends are held to one contract.
//
// Each test ledger gets its own schema, named from the path the test would have used
// for its SQLite file: opening the same path again reopens the same ledger (the
// migration tests rely on that), and the schema is dropped when the test ends.

func pgTestDSN() string { return os.Getenv("LEDGERCORE_TEST_PG") }

// onPostgres reports whether this run is against PostgreSQL.
func onPostgres() bool { return pgTestDSN() != "" }

// skipOnPostgres skips a test that is about SQLite itself (a legacy column from an
// older release, a PRAGMA, a file on disk) and has no PostgreSQL meaning.
func skipOnPostgres(t *testing.T, why string) {
	t.Helper()
	if onPostgres() {
		t.Skip("SQLite-only: " + why)
	}
}

// backendOptions returns Options that open the test ledger identified by path, on
// whichever backend this run uses.
func backendOptions(t *testing.T, path string) Options {
	t.Helper()
	dsn := pgTestDSN()
	if dsn == "" {
		return Options{Path: path}
	}
	sum := sha256.Sum256([]byte(path))
	schema := "t" + hex.EncodeToString(sum[:])[:16]
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + schema); err != nil {
		t.Fatalf("postgres: creating schema: %v", err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("pgx", dsn)
		if err == nil {
			a.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
			a.Close()
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("postgres DSN: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return Options{DSN: u.String()}
}

// assertIntegrity checks the database file itself. PostgreSQL checks its own storage;
// there is no equivalent to ask for.
func assertIntegrity(t *testing.T, db *DB) {
	t.Helper()
	if onPostgres() {
		return
	}
	var res string
	if err := db.conn.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
		t.Errorf("integrity_check = %q %v", res, err)
	}
}

// dropTrigger removes a trigger the way each database spells it.
func dropTrigger(t *testing.T, db *DB, name, table string) {
	t.Helper()
	q := `DROP TRIGGER ` + name
	if onPostgres() {
		q += ` ON ` + table
	}
	if _, err := db.conn.Exec(q); err != nil {
		t.Fatal(err)
	}
}

// triggerExists asks the database whether a trigger of that name is installed.
func triggerExists(t *testing.T, db *DB, name string) bool {
	t.Helper()
	q := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`
	if onPostgres() {
		q = `SELECT COUNT(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND NOT t.tgisinternal AND t.tgname = ?`
	}
	var n int
	if err := db.conn.QueryRow(q, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// createStaleTrigger installs a vocabulary trigger under an old name that refuses
// everything but "backlog", to prove the stale-trigger replacement.
func createStaleTrigger(t *testing.T, db *DB, name string) {
	t.Helper()
	stmts := []string{`CREATE TRIGGER ` + name + ` BEFORE INSERT ON items WHEN NEW.status NOT IN ('backlog')
		BEGIN SELECT RAISE(ABORT, 'stale vocabulary'); END`}
	if onPostgres() {
		stmts = []string{
			`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $b$ BEGIN
				IF NEW.status NOT IN ('backlog') THEN RAISE EXCEPTION 'stale vocabulary'; END IF; RETURN NEW; END $b$`,
			`CREATE TRIGGER ` + name + ` BEFORE INSERT ON items FOR EACH ROW EXECUTE FUNCTION ` + name + `()`,
		}
	}
	for _, q := range stmts {
		if _, err := db.conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackendOptions_chooseByEnvironment(t *testing.T) {
	t.Setenv("LEDGERCORE_TEST_PG", "")
	if o := backendOptions(t, "/tmp/x.db"); o.Path != "/tmp/x.db" || o.DSN != "" {
		t.Errorf("default must be SQLite: %+v", o)
	}
	if !strings.HasPrefix("t", "t") { // keep strings imported for the schema-name rule below
		t.Fatal("unreachable")
	}
}

// Several programs opening the same brand-new ledger at the same moment must all
// succeed: opening applies the schema and the migrations, and PostgreSQL's
// CREATE ... IF NOT EXISTS is not safe to race. (mcp-local opens the ledger on
// every tool call, so this is the normal case, not a corner.)
func TestOpen_concurrentFirstOpenIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	opts := backendOptions(t, path)
	const n = 8
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			s, err := Open(opts)
			if err == nil {
				err = s.Close()
			}
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("a concurrent first open failed: %v", err)
		}
	}
	// And the ledger they all opened is whole.
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.(*DB)
	cols, err := timestampColumns(ctx, db.conn.d, db.conn)
	if err != nil || len(cols) == 0 {
		t.Errorf("the schema is not complete after concurrent opens: %v %d", err, len(cols))
	}
	if !triggerExists(t, db, relationTriggerName("ins")) {
		t.Error("the vocabulary triggers are missing after concurrent opens")
	}
}
