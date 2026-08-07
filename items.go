package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Item is a unit of work, typed for full lifecycle accounting from discovery
// through deployment and support: epic, story, task, spike, plan, defect,
// release, or incident. Hierarchy is expressed via ParentID (containment);
// cross-cutting edges that aren't containment live in item_relations instead.
type Item struct {
	ID          string
	ProjectID   string
	ParentID    sql.NullString
	Type        string
	Title       string
	Description sql.NullString
	Status      string
	Label       sql.NullString
	Priority    sql.NullInt64
	Assignee    sql.NullString
	CreatedAt   string
	UpdatedAt   string
}

// CreateItemParams are the inputs to CreateItem. Type defaults to "task",
// Status defaults to "backlog", and nesting is unconstrained — ParentID is
// never validated against Type.
type CreateItemParams struct {
	ProjectID   string
	ParentID    string
	Type        string
	Title       string
	Description string
	Status      string
	Label       string
	Priority    *int
	Assignee    string
}

const itemColumns = `id, project_id, parent_id, type, title, description, status, label, priority, assignee, created_at, updated_at`

// validItemTypesDesc lists the allowed values for CreateItemParams.Type, for
// use in the rejection error message.
const validItemTypesDesc = "epic, story, task, spike, plan, defect, release, incident"

// validItemTypes covers the full work lifecycle: discovery (spike),
// planning (epic, story, plan), implementation (task), quality (defect),
// deployment (release), and support (incident). Enforced only at creation --
// Type has no update path through any tool, so this is the only place it
// needs to be checked.
var validItemTypes = map[string]bool{
	"epic": true, "story": true, "task": true, "spike": true,
	"plan": true, "defect": true, "release": true, "incident": true,
}

// validStatusesDesc lists the allowed values for CreateItemParams.Status and
// UpdateItemStatus's status, for use in rejection error messages.
const validStatusesDesc = "backlog, planned, in_progress, blocked, done"

// validStatuses mirrors validItemTypes' shape. Creating an item already-done
// (or already in_progress, etc.) is a real, common need -- e.g. logging past
// work -- so Status is a real input to CreateItem, not always hardcoded, and
// needs the same validation Type already has.
var validStatuses = map[string]bool{
	"backlog": true, "planned": true, "in_progress": true,
	"blocked": true, "done": true,
}

func scanItem(row interface{ Scan(...any) error }) (*Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.ProjectID, &it.ParentID, &it.Type, &it.Title, &it.Description,
		&it.Status, &it.Label, &it.Priority, &it.Assignee, &it.CreatedAt, &it.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// CreateItem inserts a new item (status defaults to "backlog") and records
// its creation in audit_log within the same transaction.
func (db *DB) CreateItem(ctx context.Context, p CreateItemParams) (*Item, error) {
	if p.Type == "" {
		p.Type = "task"
	}
	if !validItemTypes[p.Type] {
		return nil, fmt.Errorf("invalid type %q: must be one of %s", p.Type, validItemTypesDesc)
	}
	if p.Status == "" {
		p.Status = "backlog"
	}
	if !validStatuses[p.Status] {
		return nil, fmt.Errorf("invalid status %q: must be one of %s", p.Status, validStatusesDesc)
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	id := uuid.NewString()
	now := nowUTC()
	var priority any
	if p.Priority != nil {
		priority = *p.Priority
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO items (id, project_id, parent_id, type, title, description, status, label, priority, assignee, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.ProjectID, nullIfEmpty(p.ParentID), p.Type, p.Title, nullIfEmpty(p.Description),
		p.Status, nullIfEmpty(p.Label), priority, nullIfEmpty(p.Assignee), now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("inserting item: %w", err)
	}

	detail, _ := json.Marshal(map[string]string{"title": p.Title, "type": p.Type})
	if err := insertAudit(ctx, tx, "item", id, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return db.GetItem(ctx, id)
}

// GetItem fetches a single item by id.
func (db *DB) GetItem(ctx context.Context, id string) (*Item, error) {
	row := db.conn.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE id = ?`, id)
	it, err := scanItem(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("item %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	return it, nil
}

// ItemFilter narrows ListItems. Zero-value fields mean "no filter" on that
// column, INCLUDING ProjectID — leaving it empty means every project, not
// "no results" (the tool layer is responsible for requiring a project
// unless the caller explicitly asked for an all-projects query). ParentID
// and TopLevelOnly are mutually exclusive in intent (a specific parent vs.
// "no parent at all"); if both are set, ParentID wins and TopLevelOnly is
// ignored — narrowing to a specific parent is more specific than "top-level
// only".
type ItemFilter struct {
	ProjectID    string
	Status       string
	Label        string
	Type         string
	ParentID     string
	TopLevelOnly bool
	Assignee     string
}

// ListItems returns items matching the given filters, ordered by creation
// time — across every project if f.ProjectID is empty, or scoped to one if
// set.
func (db *DB) ListItems(ctx context.Context, f ItemFilter) ([]Item, error) {
	q := `SELECT ` + itemColumns + ` FROM items WHERE deleted_at IS NULL`
	var args []any
	if f.ProjectID != "" {
		q += ` AND project_id = ?`
		args = append(args, f.ProjectID)
	}
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Label != "" {
		q += ` AND label = ?`
		args = append(args, f.Label)
	}
	if f.Type != "" {
		q += ` AND type = ?`
		args = append(args, f.Type)
	}
	if f.Assignee != "" {
		q += ` AND assignee = ?`
		args = append(args, f.Assignee)
	}
	switch {
	case f.ParentID != "":
		q += ` AND parent_id = ?`
		args = append(args, f.ParentID)
	case f.TopLevelOnly:
		q += ` AND parent_id IS NULL`
	}
	q += ` ORDER BY created_at`

	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *it)
	}
	return items, rows.Err()
}

// FindItems does a substring, case-insensitive search over item titles.
// projectID empty means every project -- there's no FTS or title index in
// this schema (fine at this ledger's actual scale of dozens-to-low-hundreds
// of items), so a LIKE scan is the only option and deliberately not backed
// by a new dependency.
func (db *DB) FindItems(ctx context.Context, projectID, query string) ([]Item, error) {
	q := `SELECT ` + itemColumns + ` FROM items WHERE deleted_at IS NULL AND title LIKE ?`
	args := []any{"%" + query + "%"}
	if projectID != "" {
		q += ` AND project_id = ?`
		args = append(args, projectID)
	}
	q += ` ORDER BY created_at`

	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *it)
	}
	return items, rows.Err()
}

// UpdateItemStatus changes an item's status and records the before/after
// values in audit_log, in the same transaction as the update.
func (db *DB) UpdateItemStatus(ctx context.Context, id, status string) (*Item, error) {
	if !validStatuses[status] {
		return nil, fmt.Errorf("invalid status %q: must be one of %s", status, validStatusesDesc)
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var oldStatus string
	err = tx.QueryRowContext(ctx, `SELECT status FROM items WHERE id = ?`, id).Scan(&oldStatus)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("item %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}

	now := nowUTC()
	if _, err := tx.ExecContext(ctx, `UPDATE items SET status = ?, updated_at = ? WHERE id = ?`, status, now, id); err != nil {
		return nil, fmt.Errorf("updating status: %w", err)
	}

	detail, _ := json.Marshal(map[string]string{"from": oldStatus, "to": status})
	if err := insertAudit(ctx, tx, "item", id, "status_changed", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return db.GetItem(ctx, id)
}

// BulkStatusResult is one id's outcome from BulkUpdateItemStatus.
type BulkStatusResult struct {
	ID    string
	Item  *Item
	Error error
}

// BulkUpdateItemStatus applies UpdateItemStatus to each id independently --
// one bad id doesn't block the rest. Each success gets its own real
// audit_log entry, exactly as if updated individually; there is no
// bulk-specific audit shape.
func (db *DB) BulkUpdateItemStatus(ctx context.Context, ids []string, status string) []BulkStatusResult {
	results := make([]BulkStatusResult, 0, len(ids))
	for _, id := range ids {
		item, err := db.UpdateItemStatus(ctx, id, status)
		results = append(results, BulkStatusResult{ID: id, Item: item, Error: err})
	}
	return results
}

// UpdateItemPriority changes an item's priority and records the before/after
// values in audit_log, in the same transaction as the update.
func (db *DB) UpdateItemPriority(ctx context.Context, id string, priority int) (*Item, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var oldPriority sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT priority FROM items WHERE id = ?`, id).Scan(&oldPriority)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("item %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}

	now := nowUTC()
	if _, err := tx.ExecContext(ctx, `UPDATE items SET priority = ?, updated_at = ? WHERE id = ?`, priority, now, id); err != nil {
		return nil, fmt.Errorf("updating priority: %w", err)
	}

	oldVal := "(none)"
	if oldPriority.Valid {
		oldVal = fmt.Sprintf("%d", oldPriority.Int64)
	}
	detail, _ := json.Marshal(map[string]string{"from": oldVal, "to": fmt.Sprintf("%d", priority)})
	if err := insertAudit(ctx, tx, "item", id, "priority_changed", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return db.GetItem(ctx, id)
}

// UpdateItemParams is UpdateItem's per-field patch: nil means "leave this
// field unchanged." At least one field must be set. Description/Label use
// *string so an explicit empty string still clears them (nullIfEmpty, same
// convention as every other free-text field) — nil is genuinely different
// from "set to empty."
type UpdateItemParams struct {
	Title       *string
	Description *string
	Label       *string
}

// UpdateItem patches an item's title/description/label — whichever fields
// are non-nil in p — and records exactly what changed (old/new per field)
// in a single "updated" audit_log entry. A field whose new value equals its
// current value is not recorded as a change and does not appear in the
// UPDATE at all.
func (db *DB) UpdateItem(ctx context.Context, id string, p UpdateItemParams) (*Item, error) {
	if p.Title == nil && p.Description == nil && p.Label == nil {
		return nil, errors.New("at least one of title, description, or label must be given")
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var oldTitle string
	var oldDescription, oldLabel sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT title, description, label FROM items WHERE id = ?`, id).
		Scan(&oldTitle, &oldDescription, &oldLabel)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("item %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}

	changes := map[string][2]string{}
	var setClauses []string
	var args []any

	if p.Title != nil && *p.Title != oldTitle {
		changes["title"] = [2]string{oldTitle, *p.Title}
		setClauses = append(setClauses, "title = ?")
		args = append(args, *p.Title)
	}
	if p.Description != nil && *p.Description != oldDescription.String {
		changes["description"] = [2]string{oldDescription.String, *p.Description}
		setClauses = append(setClauses, "description = ?")
		args = append(args, nullIfEmpty(*p.Description))
	}
	if p.Label != nil && *p.Label != oldLabel.String {
		changes["label"] = [2]string{oldLabel.String, *p.Label}
		setClauses = append(setClauses, "label = ?")
		args = append(args, nullIfEmpty(*p.Label))
	}

	if len(setClauses) == 0 {
		return db.GetItem(ctx, id)
	}

	now := nowUTC()
	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, now, id)
	q := "UPDATE items SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return nil, fmt.Errorf("updating item: %w", err)
	}

	detail, _ := json.Marshal(changes)
	if err := insertAudit(ctx, tx, "item", id, "updated", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return db.GetItem(ctx, id)
}

// UpdateItemAssignee changes an item's assignee and records the before/after
// values in audit_log, in the same transaction as the update. An empty
// assignee clears it (unassigns), same as any other free-text field's
// nullIfEmpty convention.
func (db *DB) UpdateItemAssignee(ctx context.Context, id, assignee string) (*Item, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var oldAssignee sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT assignee FROM items WHERE id = ?`, id).Scan(&oldAssignee)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("item %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}

	now := nowUTC()
	if _, err := tx.ExecContext(ctx, `UPDATE items SET assignee = ?, updated_at = ? WHERE id = ?`, nullIfEmpty(assignee), now, id); err != nil {
		return nil, fmt.Errorf("updating assignee: %w", err)
	}

	detail, _ := json.Marshal(map[string]string{"from": oldAssignee.String, "to": assignee})
	if err := insertAudit(ctx, tx, "item", id, "assigned", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return db.GetItem(ctx, id)
}
