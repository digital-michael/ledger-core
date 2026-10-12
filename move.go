package ledgercore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// Moving work between projects.
//
// Nothing could change an item's project before this: ledger_update_item
// edits words, and recreating a ticket elsewhere loses its id, its notes and
// its history. A move changes where an item lives and nothing else, so ids,
// notes, resources, relations, workflow associations and the audit trail all
// carry on. Relations and workflow links point at item ids and have no
// project column, which is why this is small.
//
// The rules exist so a move never leaves a mess it did not announce:
//
//   - The target must already exist. A move never creates a project.
//   - An item with descendants moves only with them (WithChildren), because
//     moving the parent alone would leave children whose parent is in another
//     project.
//   - An item under a parent that is not moving is refused unless the caller
//     asks to detach it (DetachParent). Both ways are explicit (docs/ledger.md U14).
//   - Soft-deleted descendants move too and stay deleted. They keep their
//     parent link, so leaving them behind would strand them across projects.
//   - Anything the move does not change but the caller may care about is
//     counted in the result rather than silently left: workflow links, and
//     relations to items in a third project.

// MoveOptions says how far a move goes.
type MoveOptions struct {
	WithChildren bool // move every descendant with the item
	DetachParent bool // allow moving an item out from under a parent that stays behind
	DryRun       bool // work out the result and write nothing
}

// MovedItem is one item that moved, or would.
type MovedItem struct {
	ID      string
	Title   string
	Type    string
	Deleted bool
}

// MoveResult describes a move, whether or not it was a dry run.
type MoveResult struct {
	FromProject string // key
	ToProject   string // key
	Items       []MovedItem
	Notes       int  // notes and comments that followed their items
	Resources   int  // resources that followed their items
	Workflows   int  // workflow associations and links on moved items: unchanged, they refer to item ids
	Relations   int  // relations from a moved item to an item outside the move, in a project other than the target
	Detached    bool // the root was detached from a parent that stayed behind
	DryRun      bool
}

// setCTE selects the item and every descendant. UNION (not UNION ALL) so a
// parent cycle in bad data ends instead of looping. Argument: the root id.
const setCTE = `WITH RECURSIVE t(id) AS (
	SELECT id FROM items WHERE id = ?
	UNION
	SELECT i.id FROM items i JOIN t ON i.parent_id = t.id
) `

// MoveItem moves an item, and with WithChildren its descendants, to the
// project whose key is targetKey. It is one transaction, with one audit row
// per moved item sharing a batch id.
func (db *DB) MoveItem(ctx context.Context, id, targetKey string, opt MoveOptions) (*MoveResult, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var srcProjectID string
	var parentID, itemDeleted sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT project_id, parent_id, deleted_at FROM items WHERE id = ?`, id).
		Scan(&srcProjectID, &parentID, &itemDeleted)
	if err == sql.ErrNoRows {
		return nil, &lookupError{msg: fmt.Sprintf("item %q not found", id), kind: ErrNotFound}
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}
	if itemDeleted.Valid {
		return nil, errDeleted("item", id)
	}

	target, targetDeleted, err := findProject(ctx, tx, targetKey)
	if err == sql.ErrNoRows {
		return nil, &lookupError{msg: fmt.Sprintf("project %q not found; a move never creates a project (ledger_list_projects shows the keys)", targetKey), kind: ErrNotFound}
	}
	if err != nil {
		return nil, fmt.Errorf("looking up project %q: %w", targetKey, err)
	}
	if targetDeleted.Valid {
		return nil, &ErrProjectSoftDeleted{Key: target.Key, ID: target.ID}
	}
	if target.ID == srcProjectID {
		return nil, fmt.Errorf("item %q is already in project %q", id, targetKey)
	}
	var srcKey string
	if err := tx.QueryRowContext(ctx, `SELECT key FROM projects WHERE id = ?`, srcProjectID).Scan(&srcKey); err != nil {
		return nil, fmt.Errorf("looking up the item's project: %w", err)
	}

	if err := db.permit(ctx, OpMoveItem, Target{ProjectID: srcProjectID, EntityType: "item", EntityID: id}); err != nil {
		return nil, err
	}

	items, err := moveSet(ctx, tx, id, srcProjectID)
	if err != nil {
		return nil, err
	}
	if len(items) > 1 && !opt.WithChildren {
		return nil, fmt.Errorf("item %q has %d descendants; moving it alone would leave children whose parent is in another project. Move them with it (with_children=true)", id, len(items)-1)
	}
	if parentID.Valid && !opt.DetachParent {
		return nil, fmt.Errorf("item %q is under parent %q, which is not moving; that would leave a parent link across projects. Move the parent too, or detach this item (detach_parent=true)", id, parentID.String)
	}
	if parentID.Valid {
		if err := db.permit(ctx, OpReparentItem, Target{ProjectID: srcProjectID, EntityType: "item", EntityID: id}); err != nil {
			return nil, err
		}
	}
	for _, it := range items {
		if it.Type == "component" && !it.Deleted {
			if err := validateComponentTitleUnique(ctx, tx, target.ID, it.Title, ""); err != nil {
				return nil, err
			}
		}
	}

	res := &MoveResult{FromProject: srcKey, ToProject: target.Key, Items: items, Detached: parentID.Valid, DryRun: opt.DryRun}
	count := func(q string, into *int, extra ...any) error {
		return tx.QueryRowContext(ctx, setCTE+q, append([]any{id}, extra...)...).Scan(into)
	}
	if err := count(`SELECT COUNT(*) FROM notes WHERE item_id IN (SELECT id FROM t)`, &res.Notes); err != nil {
		return nil, fmt.Errorf("counting notes: %w", err)
	}
	if err := count(`SELECT COUNT(*) FROM resources WHERE item_id IN (SELECT id FROM t)`, &res.Resources); err != nil {
		return nil, fmt.Errorf("counting resources: %w", err)
	}
	if err := count(`SELECT (SELECT COUNT(*) FROM workflow_associations WHERE item_id IN (SELECT id FROM t))
		+ (SELECT COUNT(*) FROM workflow_links WHERE item_id IN (SELECT id FROM t))`, &res.Workflows); err != nil {
		return nil, fmt.Errorf("counting workflow links: %w", err)
	}
	if err := count(`SELECT COUNT(*) FROM item_relations r
		JOIN items o ON o.id = CASE WHEN r.from_item_id IN (SELECT id FROM t) THEN r.to_item_id ELSE r.from_item_id END
		WHERE r.deleted_at IS NULL
		  AND (r.from_item_id IN (SELECT id FROM t)) <> (r.to_item_id IN (SELECT id FROM t))
		  AND o.project_id <> ?`, &res.Relations, target.ID); err != nil {
		return nil, fmt.Errorf("counting relations: %w", err)
	}

	if opt.DryRun {
		return res, nil
	}

	now := nowUTC()
	exec := func(what, q string, args ...any) error {
		if _, err := tx.ExecContext(ctx, setCTE+q, append([]any{id}, args...)...); err != nil {
			return fmt.Errorf("moving %s: %w", what, err)
		}
		return nil
	}
	if err := exec("items", `UPDATE items SET project_id = ?, updated_at = ? WHERE id IN (SELECT id FROM t)`, target.ID, now); err != nil {
		return nil, err
	}
	if parentID.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE items SET parent_id = NULL WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("detaching from parent: %w", err)
		}
	}
	// A note with no project of its own stays that way; only notes that were in the old project follow.
	if err := exec("notes", `UPDATE notes SET project_id = ? WHERE item_id IN (SELECT id FROM t) AND project_id = ?`, target.ID, srcProjectID); err != nil {
		return nil, err
	}
	if err := exec("resources", `UPDATE resources SET project_id = ? WHERE item_id IN (SELECT id FROM t)`, target.ID); err != nil {
		return nil, err
	}

	auditCtx := ctx
	if len(items) > 1 && batchFromContext(ctx) == "" {
		auditCtx = WithBatch(ctx, uuid.NewString())
	}
	for _, it := range items {
		detail := map[string]string{"from_project": srcKey, "to_project": target.Key, "root": id}
		if it.ID == id && parentID.Valid {
			detail["detached_from_parent"] = parentID.String
		}
		b, _ := json.Marshal(detail)
		if err := db.insertAudit(auditCtx, tx, "item", it.ID, "moved", string(b)); err != nil {
			return nil, fmt.Errorf("writing audit log: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// moveSet lists the root and every descendant, root first. All must be in
// the root's project: an item already in a third project is a data oddity
// the operator should look at, not something to move along silently.
func moveSet(ctx context.Context, tx *sql.Tx, id, srcProjectID string) ([]MovedItem, error) {
	rows, err := tx.QueryContext(ctx, setCTE+`SELECT i.id, i.title, i.type, i.deleted_at IS NOT NULL, i.project_id
		FROM items i WHERE i.id IN (SELECT id FROM t) ORDER BY i.id = ? DESC, i.created_at, i.id`, id, id)
	if err != nil {
		return nil, fmt.Errorf("listing the items to move: %w", err)
	}
	defer rows.Close()
	var out []MovedItem
	for rows.Next() {
		var it MovedItem
		var pid string
		if err := rows.Scan(&it.ID, &it.Title, &it.Type, &it.Deleted, &pid); err != nil {
			return nil, err
		}
		if pid != srcProjectID {
			return nil, fmt.Errorf("descendant %q is in a different project from item %q; fix that first (it should not happen)", it.ID, id)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
