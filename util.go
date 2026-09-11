package ledgercore

import (
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
	return fmt.Errorf("%s %q is deleted; restore it first (ledger_restore entity_type=%s id=%s)", entityType, id, entityType, id)
}

// nullIfEmpty maps an empty string to a SQL NULL, and anything else through
// unchanged. Used for optional TEXT columns bound via database/sql.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
