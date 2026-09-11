package ledgercore

import (
	"context"
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
