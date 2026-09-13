// Package ledgercore is the ledger's persistence layer: the Store contract
// plus its only implementation today, a SQLite-backed DB. Typed query
// functions for projects, items, resources, notes, item_relations, and
// audit_log are defined against *DB throughout this package.
//
// It is the single implementation of every ledger write -- validation,
// audit-in-transaction, soft delete, id-prefix resolution -- shared by every
// program that writes the ledger: mcp-local's MCP tools and ledger-server's
// web UI. It was extracted from mcp-local/internal/store/ledger on
// 2026-09-10 precisely so those two front doors could not drift apart.
//
// It deliberately knows nothing about any caller's transport. Who performed
// a mutation is supplied by the caller through Options.Client.
package ledgercore

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// Store is the ledger persistence contract. Every tool handler in
// internal/tools/ledger depends on this interface, never on *DB directly,
// so an alternate backend is a new Factory + Store implementation away --
// no handler changes required.
type Store interface {
	// Projects
	GetOrCreateProject(ctx context.Context, key, name string) (*Project, error)
	GetOrCreateProjectForWrite(ctx context.Context, key, name string) (*Project, error)
	GetProjectByID(ctx context.Context, id string) (*Project, error)
	ListProjects(ctx context.Context) ([]Project, error)

	// Items
	CreateItem(ctx context.Context, p CreateItemParams) (*Item, error)
	GetItem(ctx context.Context, id string) (*Item, error)
	ListItems(ctx context.Context, f ItemFilter) ([]Item, error)
	FindItems(ctx context.Context, projectID, query string) ([]Item, error)
	SearchItems(ctx context.Context, projectID, query string) ([]SearchResult, error)
	// UpdateItemFields is where an item edit happens: every editable field,
	// one transaction, optional conflict check. The four below are wrappers
	// kept for the callers that already use them.
	UpdateItemFields(ctx context.Context, id string, u ItemUpdate) (*Item, error)
	UpdateItem(ctx context.Context, id string, p UpdateItemParams) (*Item, error)
	UpdateItemStatus(ctx context.Context, id, status string) (*Item, error)
	BulkUpdateItemStatus(ctx context.Context, ids []string, status string) []BulkStatusResult
	UpdateItemPriority(ctx context.Context, id string, priority int) (*Item, error)
	UpdateItemAssignee(ctx context.Context, id, assignee string) (*Item, error)

	// Resources
	AddResource(ctx context.Context, projectID, itemID, url, label string) (*Resource, error)
	ListResources(ctx context.Context, itemID, projectID string) ([]Resource, error)
	UpdateResource(ctx context.Context, id string, p UpdateResourceParams) (*Resource, error)

	// Notes
	AddNote(ctx context.Context, p AddNoteParams) (*Note, error)
	ListNotes(ctx context.Context, itemID, projectID string) ([]Note, error)
	UpdateNote(ctx context.Context, id string, p UpdateNoteParams) (*Note, error)
	StartTimer(ctx context.Context, itemID string) (*Note, error)
	StopTimer(ctx context.Context, itemID string) (*Note, error)

	// Relations
	RelateItems(ctx context.Context, fromID, toID, relationType string) (*ItemRelation, error)
	BulkRelateItems(ctx context.Context, fromIDs []string, toID, relationType string) []BulkRelateResult
	ListRelations(ctx context.Context, itemID string) ([]ItemRelation, error)
	ListProjectRelations(ctx context.Context, projectID string) ([]ItemRelation, error)

	// Cross-cutting
	HealthFindings(ctx context.Context) ([]Finding, error)
	SummarizeProject(ctx context.Context, projectID, projectKey string) (*ProjectSummary, error)
	ListAuditLog(ctx context.Context, f AuditFilter) ([]AuditEntry, error)
	SoftDelete(ctx context.Context, entityType, id string) error
	Restore(ctx context.Context, entityType, id string) error

	Close() error
}

// Factory constructs a Store. SQLiteFactory is the only implementation
// today; a MySQLFactory or PostgresFactory would satisfy the same
// interface later without changing anything that depends on Factory or
// Store.
type Factory interface {
	Open() (Store, error)
}

// Options configures Open.
type Options struct {
	// Path is the SQLite file to open. Empty means DefaultPath().
	Path string

	// Actor names whoever is performing a mutation. It is called once per
	// audit row, with the context of the call that caused it -- so a caller
	// whose identity lives in the request context (mcp-local reads the MCP
	// session's negotiated ClientInfo.Name from it) resolves it per request,
	// while a caller with a fixed identity returns a constant. nil, or an
	// Actor with no ID, records NULL rather than a guess.
	Actor func(context.Context) Actor

	// Permit can refuse a privileged operation -- see policy.go. nil allows
	// everything, which is a single operator's ledger as it stands today.
	Permit func(context.Context, Actor, Operation, Target) error
}

// SQLiteFactory creates SQLite-backed Store instances using Options.
type SQLiteFactory struct {
	Options Options
}

// Open implements Factory.
func (f SQLiteFactory) Open() (Store, error) {
	return Open(f.Options)
}

// DB wraps a SQLite connection. It implements Store.
type DB struct {
	conn   *sql.DB
	actor  func(context.Context) Actor
	policy func(context.Context, Actor, Operation, Target) error
}

// DefaultPath resolves the ledger SQLite file location.
// Uses LEDGER_DB_PATH if set; otherwise $XDG_DATA_HOME/mcp-local/ledger.db,
// falling back to ~/.local/share/mcp-local/ledger.db if XDG_DATA_HOME is unset.
//
// The "mcp-local" directory name is historical -- the ledger lived inside
// mcp-local when this path was chosen -- and is kept deliberately: every
// program that opens the ledger must resolve the SAME file, and renaming the
// directory would silently point new builds at an empty database while the
// real data sat untouched at the old path.
func DefaultPath() (string, error) {
	if p := os.Getenv("LEDGER_DB_PATH"); p != "" {
		return p, nil
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "mcp-local", "ledger.db"), nil
}

// Open opens the ledger at opts.Path (or DefaultPath() if empty).
func Open(opts Options) (Store, error) {
	return open(opts)
}

// open resolves the database path, ensures its parent directory exists,
// opens the connection, locks down file permissions, and applies the schema.
func open(opts Options) (*DB, error) {
	path := opts.Path
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	// Pragmas go in the DSN, not in a conn.Exec() after opening, for two
	// separate reasons -- both of which were found by measurement on
	// 2026-09-07, after an initial fix that used conn.Exec() failed exactly
	// the concurrency test it was written to pass:
	//
	//  1. TOO LATE. Ping() below is what actually establishes the first
	//     connection, and under contention Ping() itself returns SQLITE_BUSY
	//     -- before any Exec() could have set a busy timeout. In a 5-process
	//     test, 3 of 100 real ledger_create_item calls failed at
	//     "connecting to db" with the Exec()-based fix in place.
	//  2. WRONG SCOPE. A pragma is per-connection, but database/sql keeps a
	//     POOL. conn.Exec() applies to whichever pooled connection happens to
	//     serve that one call; every connection the pool opens later reverts
	//     to the defaults. DSN pragmas are applied by the driver to every
	//     connection it creates, which is the only correct scope for these.
	//     (This is a pre-existing bug in the foreign_keys pragma, which was
	//     set by Exec() here and therefore was never reliably on for more
	//     than one pooled connection. Moving it into the DSN fixes that too.)
	//
	// busy_timeout: SQLite allows one writer at a time and, at the default of
	// 0, a second writer gives up instantly rather than waiting its turn.
	// Measured with 5 concurrent processes x 200 transactional writes: 77 of
	// 1000 succeeded without it, 1000 of 1000 with it.
	//
	// journal_mode=WAL: lets readers proceed while a writer holds the lock,
	// ~4x faster under this database's real concurrency (one mcp-local per
	// Claude Code session, plus mcp-console). It is persisted in the file, so
	// it converts once and is a no-op on every subsequent open. It must be
	// applied after busy_timeout, since converting journal mode needs an
	// exclusive lock and is therefore the statement most likely to be blocked
	// -- the driver guarantees that ordering by pushing busy_timeout to the
	// front of the _pragma list regardless of the order given here.
	//
	// _txlock=immediate: every transaction begins as BEGIN IMMEDIATE, taking
	// the write lock up front. Without it, a transaction that READS before it
	// WRITES (every update here: look up the row, then change it) holds a read
	// snapshot and must upgrade to a write lock -- and in WAL mode SQLite
	// refuses that upgrade immediately with SQLITE_BUSY rather than invoking
	// the busy timeout (waiting could deadlock), or with SQLITE_BUSY_SNAPSHOT
	// (517) if another writer committed since the read. busy_timeout alone
	// therefore protected only write-first transactions. Found 2026-09-10 when
	// adding "is it deleted?" checks made CreateItem read first: the
	// multi-process test dropped from 500/500 to 482/500. The update paths had
	// always been exposed; the 2026-09-07 tests only exercised creates.
	// Every transaction in this package writes, so immediate costs nothing.
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_txlock=immediate"

	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connecting to db: %w", err)
	}

	// Ping() is what actually creates the file on first use, so permissions
	// are set after, not before.
	if err := os.Chmod(path, 0o600); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setting db file permissions: %w", err)
	}

	// Verify the mode actually in effect rather than assuming the DSN pragma
	// took: `PRAGMA journal_mode` reports the current mode, and a conversion
	// that loses the exclusive-lock race reports the OLD value instead of
	// erroring -- so a silent no-op and a success are indistinguishable
	// without reading it back. Not fatal either way: losing the race to a
	// peer process that already converted the file is harmless, and a working
	// delete-mode connection beats refusing to open at all. Warn and continue.
	var journalMode string
	if err := conn.QueryRow(`PRAGMA journal_mode;`).Scan(&journalMode); err != nil {
		fmt.Fprintf(os.Stderr, "ledger: could not read journal_mode (%v)\n", err)
	} else if !strings.EqualFold(journalMode, "wal") {
		fmt.Fprintf(os.Stderr, "ledger: journal_mode is %q, not WAL; concurrent writes will be slower\n", journalMode)
	}

	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading embedded schema: %w", err)
	}
	if _, err := conn.Exec(string(schema)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}

	// CREATE TABLE IF NOT EXISTS (schema.sql) only handles brand-new tables —
	// a database created before deleted_at existed needs it added explicitly.
	for _, table := range []string{"projects", "items", "resources", "notes", "item_relations"} {
		if err := ensureColumn(conn, table, "deleted_at", "TEXT"); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if err := ensureColumn(conn, "audit_log", "client", "TEXT"); err != nil {
		conn.Close()
		return nil, err
	}
	if err := ensureColumn(conn, "items", "assignee", "TEXT"); err != nil {
		conn.Close()
		return nil, err
	}
	if err := ensureColumn(conn, "resources", "updated_at", "TEXT"); err != nil {
		conn.Close()
		return nil, err
	}
	// Who did it, and whether it was part of one bulk action. Added
	// 2026-09-12; existing rows keep NULL, because an old row genuinely does
	// not know its actor and inventing one would be worse than an honest gap.
	for _, c := range []struct{ table, column string }{
		{"audit_log", "actor"},
		{"audit_log", "batch"},
		{"notes", "author"},
		{"notes", "author_label"},
	} {
		if err := ensureColumn(conn, c.table, c.column, "TEXT"); err != nil {
			conn.Close()
			return nil, err
		}
	}
	// component_id (a foreign key) was replaced by component (a plain
	// denormalized string) before any external release existed to depend on
	// the old shape -- see docs/ledger.md. A database that already ran the
	// old migration has both the stale index and column; drop them (in that
	// order -- SQLite won't drop a column an index still references) before
	// adding the new one. Both drop steps are no-ops on a database that
	// never had the old shape, including a brand-new one.
	if _, err := conn.Exec(`DROP INDEX IF EXISTS idx_items_component`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("dropping stale component index: %w", err)
	}
	if err := ensureColumnDropped(conn, "items", "component_id"); err != nil {
		conn.Close()
		return nil, err
	}
	if err := ensureColumn(conn, "items", "component", "TEXT"); err != nil {
		conn.Close()
		return nil, err
	}
	// Created here, after the migration above guarantees the column exists,
	// rather than in schema.sql -- schema.sql is applied wholesale before
	// these ensureColumn migrations run, so an index on a migrated-in column
	// would fail with "no such column" on any database that predates it.
	if _, err := conn.Exec(`CREATE INDEX IF NOT EXISTS idx_items_component ON items(component)`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("creating component index: %w", err)
	}

	// Last, after every migration: the triggers reference columns that the
	// migrations above may have just added.
	if err := ensureVocabTriggers(conn); err != nil {
		conn.Close()
		return nil, err
	}

	return &DB{conn: conn, actor: opts.Actor, policy: opts.Permit}, nil
}

// ensureColumn adds column to table if it doesn't already exist. SQLite's
// ALTER TABLE ADD COLUMN errors on a column that's already there, and has no
// "IF NOT EXISTS" form, so this checks first via PRAGMA table_info — the one
// small migration primitive this store needs, now that a column can be added
// to a table that already exists in already-created database files.
func ensureColumn(conn *sql.DB, table, column, columnType string) error {
	rows, err := conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspecting %s schema: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := conn.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, columnType)); err != nil {
		return fmt.Errorf("adding column %s.%s: %w", table, column, err)
	}
	return nil
}

// ensureColumnDropped is ensureColumn's inverse: removes column from table if
// present. Needed when a migrated-in column's shape itself changes later
// (component_id, a foreign key, replaced by component, a plain
// denormalized string) -- SQLite's ALTER TABLE DROP COLUMN has no "IF
// EXISTS" form either, so this checks PRAGMA table_info first, same as
// ensureColumn.
func ensureColumnDropped(conn *sql.DB, table, column string) error {
	rows, err := conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspecting %s schema: %w", table, err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		return nil
	}

	if _, err := conn.Exec(fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", table, column)); err != nil {
		return fmt.Errorf("dropping column %s.%s: %w", table, column, err)
	}
	return nil
}

// Close closes the underlying connection.
func (db *DB) Close() error {
	return db.conn.Close()
}
