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

// Resource is a labeled URL attached to a project or, optionally, one of its
// items.
type Resource struct {
	ID        string
	ProjectID string
	ItemID    sql.NullString
	URL       string
	Label     sql.NullString
	CreatedAt string
	UpdatedAt sql.NullString // NULL until the first UpdateResource
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
	if err := db.insertAudit(ctx, tx, "resource", id, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &Resource{
		ID: id, ProjectID: projectID,
		ItemID: sql.NullString{String: itemID, Valid: itemID != ""},
		URL:    url,
		// Label was omitted from this return value until 2026-09-10, so the
		// returned Resource reported no label even when one was stored.
		// Invisible through the MCP tool, which doesn't print it, but any
		// direct caller would have been handed a wrong record.
		Label:     sql.NullString{String: label, Valid: label != ""},
		CreatedAt: now,
	}, nil
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
			`SELECT id, project_id, item_id, url, label, created_at, updated_at
			 FROM resources WHERE item_id = ? AND deleted_at IS NULL ORDER BY created_at`, itemID)
	case projectID != "":
		rows, err = db.conn.QueryContext(ctx,
			`SELECT id, project_id, item_id, url, label, created_at, updated_at
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
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ItemID, &r.URL, &r.Label, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		resources = append(resources, r)
	}
	return resources, rows.Err()
}

// UpdateResourceParams carries the fields UpdateResource may change. Same
// convention as UpdateItemParams: a nil pointer leaves the field unchanged, a
// pointer to "" clears it. URL cannot be cleared -- a resource is a URL.
type UpdateResourceParams struct {
	URL   *string
	Label *string
}

// UpdateResource edits a resource's URL and/or label, recording the change in
// audit_log in the same transaction. Only fields that are both given and
// actually different are written; if nothing differs, the current resource is
// returned and no audit row is written, matching UpdateItem.
//
// A soft-deleted resource is refused rather than edited: restore it first.
// Editing something the ledger reports as deleted would be a silent change to
// hidden state.
func (db *DB) UpdateResource(ctx context.Context, id string, p UpdateResourceParams) (*Resource, error) {
	if p.URL == nil && p.Label == nil {
		return nil, errors.New("at least one of url or label must be given")
	}
	if p.URL != nil && *p.URL == "" {
		return nil, errors.New("url cannot be cleared: a resource is a URL -- delete the resource instead")
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var oldURL string
	var oldLabel, deletedAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT url, label, deleted_at FROM resources WHERE id = ?`, id).
		Scan(&oldURL, &oldLabel, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("resource %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up resource %q: %w", id, err)
	}
	if deletedAt.Valid {
		return nil, errDeleted("resource", id)
	}

	changes := map[string][2]string{}
	var setClauses []string
	var args []any
	if p.URL != nil && *p.URL != oldURL {
		changes["url"] = [2]string{oldURL, *p.URL}
		setClauses = append(setClauses, "url = ?")
		args = append(args, *p.URL)
	}
	if p.Label != nil && *p.Label != oldLabel.String {
		changes["label"] = [2]string{oldLabel.String, *p.Label}
		setClauses = append(setClauses, "label = ?")
		args = append(args, nullIfEmpty(*p.Label))
	}
	if len(setClauses) == 0 {
		return getResource(ctx, tx, id)
	}

	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, nowUTC(), id)
	if _, err := tx.ExecContext(ctx, "UPDATE resources SET "+strings.Join(setClauses, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, fmt.Errorf("updating resource: %w", err)
	}

	detail, _ := json.Marshal(changes)
	if err := db.insertAudit(ctx, tx, "resource", id, "updated", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	res, err := getResource(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// getResource reads one resource by exact id within tx.
func getResource(ctx context.Context, tx *sql.Tx, id string) (*Resource, error) {
	var r Resource
	err := tx.QueryRowContext(ctx,
		`SELECT id, project_id, item_id, url, label, created_at, updated_at FROM resources WHERE id = ?`, id).
		Scan(&r.ID, &r.ProjectID, &r.ItemID, &r.URL, &r.Label, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("reading resource %q: %w", id, err)
	}
	return &r, nil
}
