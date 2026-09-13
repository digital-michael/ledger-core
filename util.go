package ledgercore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// nowUTC returns the current time as an RFC3339Nano string, the format used
// for every created_at/updated_at column in this schema.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// errDeleted is the refusal for any change to a soft-deleted entity.
//
// Deleted entities are read-only until restored: Restore is the one change
// they accept. Before 2026-09-10 the item update functions checked nothing,
// so a deleted item -- hidden from every list and summary -- kept accepting
// status, priority, assignee and field changes, and came back from a restore
// with edits nobody saw happen (defect b1c028d7). The message names the exact
// command that fixes it rather than failing opaquely.
func errDeleted(entityType, id string) error {
	// lookupError keeps the message exactly as it reads today (mcp-local
	// prints it) while classifying it, so a web server can answer a refusal
	// differently from a failure.
	return &lookupError{
		msg:  fmt.Sprintf("%s %q is deleted; restore it first (ledger_restore entity_type=%s id=%s)", entityType, id, entityType, id),
		kind: ErrDeleted,
	}
}

// refuseIfItemDeleted returns errDeleted when itemID names a soft-deleted
// item. Used by every write that attaches something to an item -- a note, a
// resource, a timer event, a relation, a child -- because attaching to a
// deleted item is editing it (extended 2026-09-10 from the item-update rule).
//
// An empty or unknown id is not this function's business: it returns nil and
// leaves the caller's existing handling (usually the foreign key) to decide,
// so errors for a nonexistent id are unchanged.
func refuseIfItemDeleted(ctx context.Context, tx *sql.Tx, itemID string) error {
	return refuseIfDeleted(ctx, tx, "item", "items", itemID)
}

// refuseIfProjectDeleted is refuseIfItemDeleted for projects. mcp-local's
// tools already refuse a deleted project before reaching this package
// (GetOrCreateProjectForWrite); checking here makes the rule hold for every
// caller, including ones that pass a project id directly.
func refuseIfProjectDeleted(ctx context.Context, tx *sql.Tx, projectID string) error {
	return refuseIfDeleted(ctx, tx, "project", "projects", projectID)
}

// refuseIfDeleted: table is always one of this package's literals, never
// caller input.
func refuseIfDeleted(ctx context.Context, tx *sql.Tx, entityType, table, id string) error {
	if id == "" {
		return nil
	}
	var deletedAt sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT deleted_at FROM `+table+` WHERE id = ?`, id).Scan(&deletedAt)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("looking up %s %q: %w", entityType, id, err)
	}
	if deletedAt.Valid {
		return errDeleted(entityType, id)
	}
	return nil
}

// nullIfEmpty maps an empty string to a SQL NULL, and anything else through
// unchanged. Used for optional TEXT columns bound via database/sql.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
