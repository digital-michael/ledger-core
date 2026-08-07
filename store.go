// Package ledger provides the ledger domain's persistence contract (Store)
// plus its only implementation today: a SQLite-backed DB, built via
// SQLiteFactory. Typed query functions for projects, items, resources,
// notes, item_relations, and audit_log are defined against *DB throughout
// this package; Store exists so a future MySQL/PostgreSQL backend could
// satisfy the same contract without changing any of the 20 ledger tool
// handlers in internal/tools/ledger, which depend on Store, not *DB.
package ledger

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"

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
	UpdateItem(ctx context.Context, id string, p UpdateItemParams) (*Item, error)
	UpdateItemStatus(ctx context.Context, id, status string) (*Item, error)
	UpdateItemPriority(ctx context.Context, id string, priority int) (*Item, error)
	UpdateItemAssignee(ctx context.Context, id, assignee string) (*Item, error)

	// Resources
	AddResource(ctx context.Context, projectID, itemID, url, label string) (*Resource, error)
	ListResources(ctx context.Context, itemID, projectID string) ([]Resource, error)

	// Notes
	AddNote(ctx context.Context, p AddNoteParams) (*Note, error)
	ListNotes(ctx context.Context, itemID, projectID string) ([]Note, error)
	StartTimer(ctx context.Context, itemID string) (*Note, error)
	StopTimer(ctx context.Context, itemID string) (*Note, error)

	// Relations
	RelateItems(ctx context.Context, fromID, toID, relationType string) (*ItemRelation, error)
	ListRelations(ctx context.Context, itemID string) ([]ItemRelation, error)

	// Cross-cutting
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

// SQLiteFactory creates SQLite-backed Store instances at the location
// Path() resolves (LEDGER_DB_PATH, else XDG_DATA_HOME).
type SQLiteFactory struct{}

// Open implements Factory.
func (SQLiteFactory) Open() (Store, error) {
	return open()
}

// DB wraps a SQLite connection. It implements Store.
type DB struct {
	conn *sql.DB
}

// Path resolves the ledger SQLite file location.
// Uses LEDGER_DB_PATH if set; otherwise $XDG_DATA_HOME/mcp-local/ledger.db,
// falling back to ~/.local/share/mcp-local/ledger.db if XDG_DATA_HOME is unset.
func Path() (string, error) {
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

// Open is a convenience wrapper around SQLiteFactory{}.Open() -- kept so the
// 20 existing ledger tool handlers (each calling ledgerstore.Open()) need no
// changes; only their inferred variable type changed, from *DB to Store.
func Open() (Store, error) {
	return SQLiteFactory{}.Open()
}

// open resolves the database path, ensures its parent directory exists,
// opens the connection, locks down file permissions, and applies the schema.
func open() (*DB, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	conn, err := sql.Open("sqlite", path)
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

	if _, err := conn.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
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

	return &DB{conn: conn}, nil
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

// Close closes the underlying connection.
func (db *DB) Close() error {
	return db.conn.Close()
}
