package ledgercore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

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

// rowQuerier is what *sql.DB and *sql.Tx share, so one lookup serves both.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// findProject looks a project up by its key and, failing that, by an old key
// it was renamed from. It reports deleted_at because callers disagree about
// what a deleted project means (reads ignore it, writes refuse it). sql.ErrNoRows
// means neither a key nor an alias matched.
func findProject(ctx context.Context, q rowQuerier, key string) (p Project, deletedAt sql.NullString, err error) {
	err = q.QueryRowContext(ctx, `SELECT id, key, name, deleted_at FROM projects WHERE key = ?`, key).
		Scan(&p.ID, &p.Key, &p.Name, &deletedAt)
	if err != sql.ErrNoRows {
		return p, deletedAt, err
	}
	err = q.QueryRowContext(ctx, `SELECT p.id, p.key, p.name, p.deleted_at FROM project_aliases a
		JOIN projects p ON p.id = a.project_id WHERE a.alias_key = ?`, key).
		Scan(&p.ID, &p.Key, &p.Name, &deletedAt)
	return p, deletedAt, err
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
	p, _, err := findProject(ctx, db.conn, key)
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
	p, deletedAt, err := findProject(ctx, db.conn, key)
	if err == nil {
		if deletedAt.Valid {
			return nil, &ErrProjectSoftDeleted{Key: p.Key, ID: p.ID}
		}
		return &p, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("looking up project %q: %w", key, err)
	}
	return db.insertProject(ctx, key, name)
}

// insertProject creates a project row and records its creation in
// audit_log within the same transaction — every other entity's creation
// (item, note, resource, item_relation) is already audited this way;
// project creation was the one gap, found during a 2026-08-01 CRUD+
// integration completeness audit.
func (db *DB) insertProject(ctx context.Context, key, name string) (*Project, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := nowUTC()
	id := uuid.NewString()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO projects (id, key, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, key, name, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("creating project %q: %w", key, err)
	}

	detail, _ := json.Marshal(map[string]string{"key": key, "name": name})
	if err := db.insertAudit(ctx, tx, "project", id, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &Project{ID: id, Key: key, Name: name}, nil
}

// GetProjectByID fetches a single project by its id, regardless of
// deleted_at state — same lenient convention as GetItem's lookup-by-known-ID.
func (db *DB) GetProjectByID(ctx context.Context, id string) (*Project, error) {
	var p Project
	err := db.conn.QueryRowContext(ctx, `SELECT id, key, name FROM projects WHERE id = ?`, id).Scan(&p.ID, &p.Key, &p.Name)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("project %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
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

// maxProjectNameLen bounds a display name; keys are not bounded here.
const maxProjectNameLen = 200

// RenameProject changes a project's display name. The key is untouched, so
// git-root inference, links and every stored reference carry on working;
// changing the key needs an alias for the old one and is a separate piece of
// work (ledger-server story "Project rename with alias").
//
// A name may contain spaces and any printable character. It may not be empty,
// run over maxProjectNameLen, or contain a control character, because the
// name is printed on one line by list output.
func (db *DB) RenameProject(ctx context.Context, key, newName string) (*Project, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, errors.New("a project name cannot be empty")
	}
	if utf8.RuneCountInString(newName) > maxProjectNameLen {
		return nil, fmt.Errorf("a project name is at most %d characters", maxProjectNameLen)
	}
	for _, r := range newName {
		if unicode.IsControl(r) {
			return nil, errors.New("a project name cannot contain a control character or a line break")
		}
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	p, deletedAt, err := findProject(ctx, tx, key)
	if err == sql.ErrNoRows {
		return nil, &lookupError{msg: fmt.Sprintf("project %q not found (ledger_list_projects shows the keys)", key), kind: ErrNotFound}
	}
	if err != nil {
		return nil, fmt.Errorf("looking up project %q: %w", key, err)
	}
	if deletedAt.Valid {
		return nil, &ErrProjectSoftDeleted{Key: p.Key, ID: p.ID}
	}
	if p.Name == newName {
		return &p, nil // nothing differs: write nothing
	}
	if err := db.permit(ctx, OpRenameProject, Target{ProjectID: p.ID, EntityType: "project", EntityID: p.ID}); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE projects SET name = ?, updated_at = ? WHERE id = ?`, newName, nowUTC(), p.ID); err != nil {
		return nil, fmt.Errorf("renaming project: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"key": p.Key, "from": p.Name, "to": newName})
	if err := db.insertAudit(ctx, tx, "project", p.ID, "renamed", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.Name = newName
	return &p, nil
}

// maxProjectKeyLen bounds a key. Keys are often absolute paths.
const maxProjectKeyLen = 1024

// RenameProjectKey gives a project a new key and keeps the old one as an alias,
// so everything still using the old key (git-root inference above all) reaches
// the same project instead of creating a fresh one. The name, the items and
// every stored reference are untouched: references use the project's id.
//
// Refused: an empty or control-character key, one already used by another
// project or alias, and a deleted project. Renaming back to one of this
// project's own old keys is allowed; that alias stops being an alias.
func (db *DB) RenameProjectKey(ctx context.Context, key, newKey string) (*Project, error) {
	if newKey != strings.TrimSpace(newKey) {
		return nil, errors.New("a project key cannot start or end with a space")
	}
	if newKey == "" {
		return nil, errors.New("a project key cannot be empty")
	}
	if utf8.RuneCountInString(newKey) > maxProjectKeyLen {
		return nil, fmt.Errorf("a project key is at most %d characters", maxProjectKeyLen)
	}
	for _, r := range newKey {
		if unicode.IsControl(r) {
			return nil, errors.New("a project key cannot contain a control character or a line break")
		}
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	p, deletedAt, err := findProject(ctx, tx, key)
	if err == sql.ErrNoRows {
		return nil, &lookupError{msg: fmt.Sprintf("project %q not found (ledger_list_projects shows the keys)", key), kind: ErrNotFound}
	}
	if err != nil {
		return nil, fmt.Errorf("looking up project %q: %w", key, err)
	}
	if deletedAt.Valid {
		return nil, &ErrProjectSoftDeleted{Key: p.Key, ID: p.ID}
	}
	if p.Key == newKey {
		return &p, nil // nothing differs: write nothing
	}

	// The new key must not already mean something else.
	if other, _, err := findProject(ctx, tx, newKey); err == nil {
		if other.ID != p.ID {
			return nil, fmt.Errorf("key %q is already used by project %q (id=%s); a key or an old key can belong to one project only", newKey, other.Key, other.ID)
		}
	} else if err != sql.ErrNoRows {
		return nil, fmt.Errorf("checking key %q: %w", newKey, err)
	}

	if err := db.permit(ctx, OpRenameProject, Target{ProjectID: p.ID, EntityType: "project", EntityID: p.ID}); err != nil {
		return nil, err
	}

	// Renaming back to one of its own old keys: that alias becomes the key.
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_aliases WHERE alias_key = ? AND project_id = ?`, newKey, p.ID); err != nil {
		return nil, fmt.Errorf("releasing the alias: %w", err)
	}
	now := nowUTC()
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET key = ?, updated_at = ? WHERE id = ?`, newKey, now, p.ID); err != nil {
		return nil, fmt.Errorf("renaming the key: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_aliases (alias_key, project_id, created_at) VALUES (?, ?, ?)`, p.Key, p.ID, now); err != nil {
		return nil, fmt.Errorf("keeping the old key as an alias: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"from_key": p.Key, "to_key": newKey})
	if err := db.insertAudit(ctx, tx, "project", p.ID, "key_renamed", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.Key = newKey
	return &p, nil
}

// ProjectKeyForAlias reports the current key of the project that once had alias
// as its key. ok is false when alias is not an old key of any project.
func (db *DB) ProjectKeyForAlias(ctx context.Context, alias string) (key string, ok bool, err error) {
	err = db.conn.QueryRowContext(ctx, `SELECT p.key FROM project_aliases a JOIN projects p ON p.id = a.project_id
		WHERE a.alias_key = ? AND p.deleted_at IS NULL`, alias).Scan(&key)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// ProjectAliases lists a project's old keys, oldest first.
func (db *DB) ProjectAliases(ctx context.Context, projectID string) ([]string, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT alias_key FROM project_aliases WHERE project_id = ? ORDER BY created_at, alias_key`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing aliases: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
