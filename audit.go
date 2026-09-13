package ledgercore

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// AuditEntry is one row of the audit_log — a record of a mutation or sync
// attempt, written centrally by the functions in this package, never by
// callers directly.
type AuditEntry struct {
	ID         string
	EntityType string
	EntityID   sql.NullString
	Operation  string
	Detail     sql.NullString
	CreatedAt  string
	Client     sql.NullString // readable label, e.g. "claude-code"
	Actor      sql.NullString // canonical "kind:id"; NULL before 2026-09-12
	Batch      sql.NullString // set when this row was part of one bulk action
}

// insertAudit writes one audit_log row as part of tx. Every mutating
// function in this package calls this inside its own transaction, so the
// audit row and the change it describes commit or roll back together.
//
// The actor comes from Options.Actor, resolved against ctx here, once,
// rather than threading a "who called this" value through every mutating
// function's signature. This package used to read it straight out of an
// mcp-go session in ctx, which tied the storage layer to one caller's
// transport; the caller now supplies the resolver.
//
// Two columns, deliberately: client holds the readable label (what the
// existing rows and every reader already expect), actor holds the canonical
// "kind:id" that a future account system can resolve. batch ties the rows of
// one bulk action together.
func (db *DB) insertAudit(ctx context.Context, tx *sql.Tx, entityType, entityID, operation, detail string) error {
	actor := db.currentActor(ctx)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (id, entity_type, entity_id, operation, detail, created_at, client, actor, batch)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), entityType, nullIfEmpty(entityID), operation, nullIfEmpty(detail), nowUTC(),
		nullIfEmpty(actor.DisplayName()), nullIfEmpty(actor.String()), nullIfEmpty(batchFromContext(ctx)),
	)
	return err
}

// startOfTodayUTC returns today's midnight in UTC, formatted the same way
// every created_at in this schema is — matching nowUTC()'s convention (UTC
// throughout, no local-timezone awareness anywhere else in this store).
func startOfTodayUTC() string {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return start.Format(time.RFC3339Nano)
}

// AuditEntry.Actor and .Batch expose the two columns added 2026-09-12; both
// are NULL on rows written before then.

// AuditFilter narrows ListAuditLog. Zero-value fields mean "no filter" on
// that column. Since/Today/Hours are three alternate ways to express the
// same kind of thing (a lower bound on created_at) — combining more than one
// is harmless, not an error: each just adds its own "AND created_at >= ?",
// and the most restrictive one wins naturally.
type AuditFilter struct {
	EntityType string
	EntityID   string
	Operation  string
	Since      string // RFC3339 string; rows with created_at >= Since
	Today      bool   // rows with created_at >= today's midnight, UTC
	Hours      int    // rows with created_at >= now - Hours; 0 (or negative) means "not set"
}

// ListAuditLog returns matching audit_log rows ordered by created_at.
func (db *DB) ListAuditLog(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	q := `SELECT id, entity_type, entity_id, operation, detail, created_at, client, actor, batch FROM audit_log WHERE 1=1`
	var args []any
	if f.EntityType != "" {
		q += ` AND entity_type = ?`
		args = append(args, f.EntityType)
	}
	if f.EntityID != "" {
		q += ` AND entity_id = ?`
		args = append(args, f.EntityID)
	}
	if f.Operation != "" {
		q += ` AND operation = ?`
		args = append(args, f.Operation)
	}
	if f.Since != "" {
		q += ` AND created_at >= ?`
		args = append(args, f.Since)
	}
	if f.Today {
		q += ` AND created_at >= ?`
		args = append(args, startOfTodayUTC())
	}
	if f.Hours > 0 {
		q += ` AND created_at >= ?`
		args = append(args, time.Now().UTC().Add(-time.Duration(f.Hours)*time.Hour).Format(time.RFC3339Nano))
	}
	q += ` ORDER BY created_at`

	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.EntityType, &e.EntityID, &e.Operation, &e.Detail, &e.CreatedAt, &e.Client, &e.Actor, &e.Batch); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
