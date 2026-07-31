package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Resource is a labeled URL attached to a project or, optionally, one of its
// items.
type Resource struct {
	ID        string
	ProjectID string
	ItemID    sql.NullString
	URL       string
	Label     sql.NullString
	CreatedAt string
}

// AddResource inserts a resource and records its creation in audit_log.
// itemID may be empty for a project-level resource.
func (db *DB) AddResource(ctx context.Context, projectID, itemID, url, label string) (*Resource, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	id := uuid.NewString()
	now := nowUTC()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO resources (id, project_id, item_id, url, label, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, projectID, nullIfEmpty(itemID), url, nullIfEmpty(label), now,
	)
	if err != nil {
		return nil, fmt.Errorf("inserting resource: %w", err)
	}

	detail, _ := json.Marshal(map[string]string{"url": url, "label": label})
	if err := insertAudit(ctx, tx, "resource", id, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &Resource{ID: id, ProjectID: projectID, ItemID: sql.NullString{String: itemID, Valid: itemID != ""}, URL: url, CreatedAt: now}, nil
}

// ListResources returns resources for an item, or if itemID is empty,
// project-level resources (item_id IS NULL). Exactly one of itemID/projectID
// must be non-empty — same convention as ListNotes.
func (db *DB) ListResources(ctx context.Context, itemID, projectID string) ([]Resource, error) {
	var rows *sql.Rows
	var err error
	switch {
	case itemID != "":
		rows, err = db.conn.QueryContext(ctx,
			`SELECT id, project_id, item_id, url, label, created_at
			 FROM resources WHERE item_id = ? AND deleted_at IS NULL ORDER BY created_at`, itemID)
	case projectID != "":
		rows, err = db.conn.QueryContext(ctx,
			`SELECT id, project_id, item_id, url, label, created_at
			 FROM resources WHERE project_id = ? AND item_id IS NULL AND deleted_at IS NULL ORDER BY created_at`, projectID)
	default:
		return nil, errors.New("ledger_list_resources requires an item id or a project id")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var resources []Resource
	for rows.Next() {
		var r Resource
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ItemID, &r.URL, &r.Label, &r.CreatedAt); err != nil {
			return nil, err
		}
		resources = append(resources, r)
	}
	return resources, rows.Err()
}
