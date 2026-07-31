package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// Item is a unit of work: an epic, story, or task. Hierarchy is expressed
// via ParentID (containment); cross-cutting edges that aren't containment
// live in item_relations instead.
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
	CreatedAt   string
	UpdatedAt   string
}

// CreateItemParams are the inputs to CreateItem. Type defaults to "task" and
// nesting is unconstrained — ParentID is never validated against Type.
type CreateItemParams struct {
	ProjectID   string
	ParentID    string
	Type        string
	Title       string
	Description string
	Label       string
	Priority    *int
}

const itemColumns = `id, project_id, parent_id, type, title, description, status, label, priority, created_at, updated_at`

func scanItem(row interface{ Scan(...any) error }) (*Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.ProjectID, &it.ParentID, &it.Type, &it.Title, &it.Description,
		&it.Status, &it.Label, &it.Priority, &it.CreatedAt, &it.UpdatedAt)
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
		`INSERT INTO items (id, project_id, parent_id, type, title, description, status, label, priority, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'backlog', ?, ?, ?, ?)`,
		id, p.ProjectID, nullIfEmpty(p.ParentID), p.Type, p.Title, nullIfEmpty(p.Description),
		nullIfEmpty(p.Label), priority, now, now,
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
// column; ProjectID is always required. ParentID and TopLevelOnly are
// mutually exclusive in intent (a specific parent vs. "no parent at all");
// if both are set, ParentID wins and TopLevelOnly is ignored — narrowing to
// a specific parent is more specific than "top-level only".
type ItemFilter struct {
	ProjectID    string
	Status       string
	Label        string
	Type         string
	ParentID     string
	TopLevelOnly bool
}

// ListItems returns items in a project matching the given filters, ordered
// by creation time.
func (db *DB) ListItems(ctx context.Context, f ItemFilter) ([]Item, error) {
	q := `SELECT ` + itemColumns + ` FROM items WHERE project_id = ? AND deleted_at IS NULL`
	args := []any{f.ProjectID}
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

// UpdateItemStatus changes an item's status and records the before/after
// values in audit_log, in the same transaction as the update.
func (db *DB) UpdateItemStatus(ctx context.Context, id, status string) (*Item, error) {
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
