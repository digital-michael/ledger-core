package ledger

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// Project is a registered project — the multi-project scoping unit every
// item, resource, and note belongs to.
type Project struct {
	ID   string
	Key  string
	Name string
}

// ErrProjectSoftDeleted is returned by GetOrCreateProject when key matches a
// soft-deleted project. Deliberately not auto-fixed: silently reviving it
// would be a state change (undoing a delete) hidden inside what looks like
// an unrelated read/create call — surprising, and hard to notice happened at
// all. Surfacing this as an error instead means the caller sees exactly what
// happened and makes the call themselves via ledger_restore.
type ErrProjectSoftDeleted struct {
	Key string
	ID  string
}

func (e *ErrProjectSoftDeleted) Error() string {
	return fmt.Sprintf(
		"project %q is soft-deleted (id=%s) — run ledger_restore entity_type=project id=%s to restore it, then retry",
		e.Key, e.ID, e.ID,
	)
}

// GetOrCreateProject returns the project matching key, creating one with the
// given name if it doesn't exist yet, regardless of its deleted_at state.
//
// This lenient form is for READ paths (ledger_list_items/list_notes/
// list_resources): the no-cascade guarantee ("deleting a project doesn't
// touch its items") means a soft-deleted project's existing data must stay
// readable, so lookups here deliberately ignore deleted_at entirely — same
// as looking an item up by its known ID does.
func (db *DB) GetOrCreateProject(ctx context.Context, key, name string) (*Project, error) {
	var p Project
	err := db.conn.QueryRowContext(ctx,
		`SELECT id, key, name FROM projects WHERE key = ?`, key,
	).Scan(&p.ID, &p.Key, &p.Name)
	if err == nil {
		return &p, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("looking up project %q: %w", key, err)
	}
	return db.insertProject(ctx, key, name)
}

// GetOrCreateProjectForWrite is GetOrCreateProject's stricter counterpart,
// for paths that attach NEW data under a project key (ledger_create_item,
// and project-level ledger_add_note/ledger_add_resource): if key matches a
// soft-deleted project, it returns ErrProjectSoftDeleted instead of
// silently reviving or reusing the dead row.
//
// Reviving automatically would be a state change (undoing a delete) hidden
// inside what looks like an unrelated create call — surprising, and easy to
// miss having happened at all. Erroring instead means the caller sees
// exactly what happened and makes the restore an explicit, visible action.
func (db *DB) GetOrCreateProjectForWrite(ctx context.Context, key, name string) (*Project, error) {
	var p Project
	var deletedAt sql.NullString
	err := db.conn.QueryRowContext(ctx,
		`SELECT id, key, name, deleted_at FROM projects WHERE key = ?`, key,
	).Scan(&p.ID, &p.Key, &p.Name, &deletedAt)
	if err == nil {
		if deletedAt.Valid {
			return nil, &ErrProjectSoftDeleted{Key: key, ID: p.ID}
		}
		return &p, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("looking up project %q: %w", key, err)
	}
	return db.insertProject(ctx, key, name)
}

func (db *DB) insertProject(ctx context.Context, key, name string) (*Project, error) {
	now := nowUTC()
	id := uuid.NewString()
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO projects (id, key, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, key, name, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("creating project %q: %w", key, err)
	}
	return &Project{ID: id, Key: key, Name: name}, nil
}

// ListProjects returns every registered project, ordered by key.
func (db *DB) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT id, key, name FROM projects WHERE deleted_at IS NULL ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Key, &p.Name); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}
