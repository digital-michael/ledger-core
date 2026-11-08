package ledgercore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The SQL in this package is written once (see rebind.go). What cannot be shared is
// the handful of things that are not SQL the two databases agree on: asking a
// database what tables and columns it has, spotting a value that is not a
// fixed-width stamp, "insert unless it is already there", and installing the
// triggers that enforce the vocabulary. Those live behind this interface, one
// implementation per database, so the rest of the package never asks which one it
// is talking to.
type dialect interface {
	name() string

	// tables lists the ledger's tables, excluding the database's own.
	tables(ctx context.Context, q querier) ([]string, error)
	// columns lists a table's columns in order; PK marks the primary key column(s).
	columns(ctx context.Context, q querier, table string) ([]columnInfo, error)

	// notStamp is an SQL condition that is true when col does not hold a fixed-width
	// stamp (stamps.go).
	notStamp(col string) string
	// insertIgnore is "insert this row unless a row with the same key exists", with
	// one "?" per column.
	insertIgnore(table string, columns ...string) string

	// ensureVocab installs the triggers that enforce the vocabulary (enforce.go).
	ensureVocab(ctx context.Context, c *dbConn) error

	// lockInit serialises initialisation (schema, migrations, triggers) across the
	// programs opening the same ledger at once, and returns the function that
	// releases it. SQLite's own locking already does this.
	lockInit(ctx context.Context, c *dbConn) (release func(), err error)

	// applySchema runs schema.sql, which is written once for both databases.
	applySchema(ctx context.Context, c *dbConn, schema string) error
	// columnDef is the definition of a column added to an existing table. A stamp
	// column (*_at) is declared byte-ordered where the database would otherwise
	// sort by locale.
	columnDef(column, typ string) string
}

// querier is what both *sql.DB and *sql.Tx offer for a read.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// columnInfo describes one column.
type columnInfo struct {
	Name string
	PK   bool
}

// dbConn is the database handle with its dialect. It embeds *sql.DB, so everything
// that used the plain handle (db.conn.QueryContext and the rest) is unchanged.
type dbConn struct {
	*sql.DB
	d dialect
}

// primaryKey returns the single primary key column of a table, or an error: the
// ledger's tables each have exactly one.
func primaryKey(cols []columnInfo, table string) (string, error) {
	pk := ""
	for _, c := range cols {
		if c.PK {
			if pk != "" {
				return "", fmt.Errorf("table %s has a composite primary key", table)
			}
			pk = c.Name
		}
	}
	if pk == "" {
		return "", fmt.Errorf("table %s has no primary key", table)
	}
	return pk, nil
}

// ---- SQLite ----

type sqliteDialect struct{}

func (sqliteDialect) name() string { return "sqlite" }

func (sqliteDialect) tables(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (sqliteDialect) columns(ctx context.Context, q querier, table string) ([]columnInfo, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("inspecting %s schema: %w", table, err)
	}
	defer rows.Close()
	var out []columnInfo
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, columnInfo{Name: name, PK: pk > 0})
	}
	return out, rows.Err()
}

func (sqliteDialect) notStamp(col string) string {
	return fmt.Sprintf("%s NOT GLOB '%s'", col, stampGlob)
}

func (sqliteDialect) insertIgnore(table string, columns ...string) string {
	return fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)", table, strings.Join(columns, ", "), placeholders(len(columns)))
}

func (sqliteDialect) ensureVocab(_ context.Context, c *dbConn) error { return ensureVocabTriggers(c) }

func (sqliteDialect) lockInit(context.Context, *dbConn) (func(), error) { return func() {}, nil }

func (sqliteDialect) applySchema(ctx context.Context, c *dbConn, schema string) error {
	_, err := c.ExecContext(ctx, schema)
	return err
}

func (sqliteDialect) columnDef(_, typ string) string { return typ }

// placeholders returns "?, ?, ?" for n parameters.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
