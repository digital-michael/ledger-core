package ledgercore

import (
	"context"
	"database/sql"
	"fmt"
)

// deletableTables maps a public entity_type name to its backing table.
// audit_log is deliberately excluded — it's the append-only record of what
// happened, including these very deletes, so it isn't itself deletable. This
// map is also the injection guard: table names below are interpolated into
// SQL, but only ever from this fixed literal set, never from caller input
// directly — an unrecognized entityType is rejected before reaching SQL.
var deletableTables = map[string]string{
	"project":       "projects",
	"item":          "items",
	"resource":      "resources",
	"note":          "notes",
	"item_relation": "item_relations",
}

// SoftDelete marks an entity as deleted (deleted_at = now) without removing
// the row, so it can be restored later via Restore. No cascade — deleting a
// project or item does not affect its children; this exists for test-data
// cleanup, not full referential-integrity semantics.
func (db *DB) SoftDelete(ctx context.Context, entityType, id string) error {
	table, ok := deletableTables[entityType]
	if !ok {
		return fmt.Errorf("unknown entity_type %q", entityType)
	}
	if err := db.permitDelete(ctx, entityType, id, OpDeleteProject, OpDeleteOthersNote); err != nil {
		return err
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, table),
		nowUTC(), id,
	)
	if err != nil {
		return fmt.Errorf("deleting %s %q: %w", entityType, id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%s %q not found or already deleted", entityType, id)
	}

	if err := db.insertAudit(ctx, tx, entityType, id, "deleted", ""); err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	return tx.Commit()
}

// Restore reverses a SoftDelete (deleted_at = NULL). Find candidates via
// ListAuditLog(operation="deleted") — there is no separate "list deleted"
// query; the audit trail already answers that question.
func (db *DB) Restore(ctx context.Context, entityType, id string) error {
	table, ok := deletableTables[entityType]
	if !ok {
		return fmt.Errorf("unknown entity_type %q", entityType)
	}
	if err := db.permitDelete(ctx, entityType, id, OpRestoreProject, OpDeleteOthersNote); err != nil {
		return err
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`, table),
		id,
	)
	if err != nil {
		return fmt.Errorf("restoring %s %q: %w", entityType, id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%s %q not found or not deleted", entityType, id)
	}

	if err := db.insertAudit(ctx, tx, entityType, id, "restored", ""); err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	return tx.Commit()
}

// permitDelete asks the policy about the two delete/restore cases that carry
// weight: a project (which reaches every ticket in it) and a note somebody
// else wrote. Deleting your own ticket or your own comment is ordinary work
// and is not gated -- it is soft, restorable, and the UI offers undo.
func (db *DB) permitDelete(ctx context.Context, entityType, id string, projectOp, noteOp Operation) error {
	switch entityType {
	case "project":
		return db.permit(ctx, projectOp, Target{ProjectID: id, EntityType: entityType, EntityID: id})
	case "note":
		mine, author, err := db.noteIsMine(ctx, id)
		if err != nil || mine {
			return err
		}
		return db.permit(ctx, noteOp, Target{EntityType: entityType, EntityID: id, ProjectID: author})
	}
	return nil
}

// noteIsMine reports whether the current actor wrote this note. A note with
// no recorded author (every note written before 2026-09-12) counts as mine:
// treating history as someone else's would lock the operator out of their own
// ledger.
func (db *DB) noteIsMine(ctx context.Context, id string) (bool, string, error) {
	var author sql.NullString
	err := db.conn.QueryRowContext(ctx, `SELECT author FROM notes WHERE id = ?`, id).Scan(&author)
	if err == sql.ErrNoRows {
		return true, "", nil // not found: let the caller's own error handling speak
	}
	if err != nil {
		return false, "", err
	}
	if !author.Valid || author.String == "" {
		return true, "", nil
	}
	return author.String == db.currentActor(ctx).String(), author.String, nil
}
