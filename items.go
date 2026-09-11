package ledgercore

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
// release, incident, or component. Hierarchy is expressed via ParentID
// (containment); cross-cutting edges that aren't containment live in
// item_relations instead. Component is a third, orthogonal axis: which part
// of the system this item touches (a specification-level marker, zero-or-
// one, distinct from both containment and the free-form Label). It is a
// plain denormalized string copied from an existing type=component item's
// title at assignment time -- not a foreign key -- deliberately, since
// project reorganization (merging/splitting projects, eventual cross-
// instance sync) is expected work, and a string needs no reference
// reconciliation when data moves; a stored id would. The tradeoff, accepted:
// renaming a component's pool entry does not retroactively update items that
// already copied its old title.
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
	Component   sql.NullString
	CreatedAt   string
	UpdatedAt   string
}

// CreateItemParams are the inputs to CreateItem. Type defaults to "task",
// Status defaults to "backlog", and nesting is unconstrained — ParentID is
// never validated against Type. Component, if given, must exactly match an
// existing type=component item's title (any project) at write time --
// validated then copied as a plain string, not kept as a live reference.
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
	Component   string
}

const itemColumns = `id, project_id, parent_id, type, title, description, status, label, priority, assignee, component, created_at, updated_at`

// validateComponentTitle confirms title exactly matches some existing,
// non-deleted type=component item, in any project -- component assignment
// is deliberately cross-project-tolerant, matching item_relations. Unlike
// the earlier id-based version this replaced, there is no ambiguity to
// resolve here: Component is a plain copied string, not a reference to one
// specific row, so it doesn't matter WHICH project's pool entry the title
// came from, only that it's a real, currently-live component name
// somewhere. deleted_at IS checked here (unlike RelateItems' precedent for
// live references) because this is a one-time snapshot copy, not a
// persistent pointer -- copying a retired component's name is more likely a
// mistake than a deliberate reference to preserve.
func validateComponentTitle(ctx context.Context, tx *sql.Tx, title string) error {
	var count int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM items WHERE type = 'component' AND title = ? AND deleted_at IS NULL`, title,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("checking component %q: %w", title, err)
	}
	if count == 0 {
		return fmt.Errorf("no component titled %q exists in any project's pool", title)
	}
	return nil
}

// validateComponentTitleUnique rejects creating/renaming a type=component
// item into a title that collides with another (non-deleted) component
// already in the same project. Components are a curated, named pool --
// unlike ordinary items, whose titles are never constrained -- and matters
// even without an id-based reference: a project whose own pool has two
// entries both titled "Auth" is just as confusing to a human picking a
// value as it would be to a lookup-by-id. excludeID is the item being
// updated/excluded from its own collision check; pass "" when creating (no
// real item has an empty id, so the exclusion is a harmless no-op there).
func validateComponentTitleUnique(ctx context.Context, tx *sql.Tx, projectID, title, excludeID string) error {
	var existingID string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM items WHERE type = 'component' AND project_id = ? AND title = ? AND deleted_at IS NULL AND id != ?`,
		projectID, title, excludeID,
	).Scan(&existingID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking component title uniqueness: %w", err)
	}
	return fmt.Errorf("a component titled %q already exists in this project (id=%s)", title, existingID)
}

func scanItem(row interface{ Scan(...any) error }) (*Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.ProjectID, &it.ParentID, &it.Type, &it.Title, &it.Description,
		&it.Status, &it.Label, &it.Priority, &it.Assignee, &it.Component, &it.CreatedAt, &it.UpdatedAt)
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

	// A new child changes its parent; a new item changes its project.
	if err := refuseIfItemDeleted(ctx, tx, p.ParentID); err != nil {
		return nil, err
	}
	if err := refuseIfProjectDeleted(ctx, tx, p.ProjectID); err != nil {
		return nil, err
	}
	if p.Component != "" {
		if err := validateComponentTitle(ctx, tx, p.Component); err != nil {
			return nil, err
		}
	}
	if p.Type == "component" {
		if err := validateComponentTitleUnique(ctx, tx, p.ProjectID, p.Title, ""); err != nil {
			return nil, err
		}
	}

	id := uuid.NewString()
	now := nowUTC()
	var priority any
	if p.Priority != nil {
		priority = *p.Priority
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO items (id, project_id, parent_id, type, title, description, status, label, priority, assignee, component, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.ProjectID, nullIfEmpty(p.ParentID), p.Type, p.Title, nullIfEmpty(p.Description),
		p.Status, nullIfEmpty(p.Label), priority, nullIfEmpty(p.Assignee), nullIfEmpty(p.Component), now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("inserting item: %w", err)
	}

	detail, _ := json.Marshal(map[string]string{"title": p.Title, "type": p.Type})
	if err := db.insertAudit(ctx, tx, "item", id, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return db.GetItem(ctx, id)
}

// resolveItemID resolves id to an exact item id: an exact match first, then
// a unique-prefix match if no exact match exists -- mirrors git's short-hash
// resolution, since ids are commonly truncated in conversation/doc
// references (e.g. "ticket 37f26083") but ledger_get_item historically
// required the full UUID. Errors clearly, listing every candidate, if the
// prefix matches more than one item rather than silently picking one.
// Deliberately does not filter deleted_at -- matches GetItem's own existing
// behavior of being able to fetch a soft-deleted item (e.g. to inspect it
// before ledger_restore).
func (db *DB) resolveItemID(ctx context.Context, id string) (string, error) {
	var exact string
	err := db.conn.QueryRowContext(ctx, `SELECT id FROM items WHERE id = ?`, id).Scan(&exact)
	if err == nil {
		return exact, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	rows, err := db.conn.QueryContext(ctx, `SELECT id, title FROM items WHERE id LIKE ? || '%'`, id)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type candidate struct{ id, title string }
	var matches []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.title); err != nil {
			return "", err
		}
		matches = append(matches, c)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("item %q not found", id)
	case 1:
		return matches[0].id, nil
	default:
		var sb strings.Builder
		fmt.Fprintf(&sb, "id %q matches more than one item, be more specific:\n", id)
		for _, m := range matches {
			fmt.Fprintf(&sb, "  %s %q\n", m.id, m.title)
		}
		return "", errors.New(strings.TrimRight(sb.String(), "\n"))
	}
}

// GetItem fetches a single item by id -- either an exact id or a unique
// prefix of one (see resolveItemID).
func (db *DB) GetItem(ctx context.Context, id string) (*Item, error) {
	resolvedID, err := db.resolveItemID(ctx, id)
	if err != nil {
		return nil, err
	}
	row := db.conn.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE id = ?`, resolvedID)
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
	Component    string
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
	if f.Component != "" {
		q += ` AND component = ?`
		args = append(args, f.Component)
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
	var deletedAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT status, deleted_at FROM items WHERE id = ?`, id).Scan(&oldStatus, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("item %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}
	if deletedAt.Valid {
		return nil, errDeleted("item", id)
	}

	now := nowUTC()
	if _, err := tx.ExecContext(ctx, `UPDATE items SET status = ?, updated_at = ? WHERE id = ?`, status, now, id); err != nil {
		return nil, fmt.Errorf("updating status: %w", err)
	}

	detail, _ := json.Marshal(map[string]string{"from": oldStatus, "to": status})
	if err := db.insertAudit(ctx, tx, "item", id, "status_changed", string(detail)); err != nil {
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
	var deletedAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT priority, deleted_at FROM items WHERE id = ?`, id).Scan(&oldPriority, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("item %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}
	if deletedAt.Valid {
		return nil, errDeleted("item", id)
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
	if err := db.insertAudit(ctx, tx, "item", id, "priority_changed", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return db.GetItem(ctx, id)
}

// UpdateItemParams is UpdateItem's per-field patch: nil means "leave this
// field unchanged." At least one field must be set. Description/Label/
// Component use *string so an explicit empty string still clears them
// (nullIfEmpty, same convention as every other free-text field) — nil is
// genuinely different from "set to empty." A non-empty Component is
// validated the same way CreateItem validates it (must exactly match some
// existing type=component item's title, any project).
type UpdateItemParams struct {
	Title       *string
	Description *string
	Label       *string
	Component   *string
}

// UpdateItem patches an item's title/description/label/component —
// whichever fields are non-nil in p — and records exactly what changed
// (old/new per field) in a single "updated" audit_log entry. A field whose
// new value equals its current value is not recorded as a change and does
// not appear in the UPDATE at all.
func (db *DB) UpdateItem(ctx context.Context, id string, p UpdateItemParams) (*Item, error) {
	if p.Title == nil && p.Description == nil && p.Label == nil && p.Component == nil {
		return nil, errors.New("at least one of title, description, label, or component must be given")
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var projectID, itemType, oldTitle string
	var oldDescription, oldLabel, oldComponent, deletedAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT project_id, type, title, description, label, component, deleted_at FROM items WHERE id = ?`, id).
		Scan(&projectID, &itemType, &oldTitle, &oldDescription, &oldLabel, &oldComponent, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("item %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}
	if deletedAt.Valid {
		return nil, errDeleted("item", id)
	}

	changes := map[string][2]string{}
	var setClauses []string
	var args []any

	if p.Title != nil && *p.Title != oldTitle {
		if itemType == "component" {
			if err := validateComponentTitleUnique(ctx, tx, projectID, *p.Title, id); err != nil {
				return nil, err
			}
		}
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
	if p.Component != nil && *p.Component != oldComponent.String {
		if *p.Component != "" {
			if err := validateComponentTitle(ctx, tx, *p.Component); err != nil {
				return nil, err
			}
		}
		changes["component"] = [2]string{oldComponent.String, *p.Component}
		setClauses = append(setClauses, "component = ?")
		args = append(args, nullIfEmpty(*p.Component))
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
	if err := db.insertAudit(ctx, tx, "item", id, "updated", string(detail)); err != nil {
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
	var deletedAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT assignee, deleted_at FROM items WHERE id = ?`, id).Scan(&oldAssignee, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("item %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}
	if deletedAt.Valid {
		return nil, errDeleted("item", id)
	}

	now := nowUTC()
	if _, err := tx.ExecContext(ctx, `UPDATE items SET assignee = ?, updated_at = ? WHERE id = ?`, nullIfEmpty(assignee), now, id); err != nil {
		return nil, fmt.Errorf("updating assignee: %w", err)
	}

	detail, _ := json.Marshal(map[string]string{"from": oldAssignee.String, "to": assignee})
	if err := db.insertAudit(ctx, tx, "item", id, "assigned", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return db.GetItem(ctx, id)
}
