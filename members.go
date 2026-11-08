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

// Project members (decided 2026-11-07).
//
// A project has no single "owner" column: ownership and access live here, so
// there can be several owners. Each live row is one person or agent with a
// role -- owner, member or viewer. The rules:
//
//   - a member is an identity "kind:id" (identity.go): a person or an agent;
//   - only a person can be an owner -- ownership is accountability;
//   - a member is unique per project among live rows (a partial unique index),
//     so a removed member can be added again;
//   - a project that has an owner keeps one: the last owner cannot be removed
//     or demoted (ErrLastOwner), and the generic soft delete cannot be a way
//     round that;
//   - a person who creates a project becomes its first owner. An agent or a
//     program that creates one is recorded as the creator only, and the project
//     has no owner until a person is added.

// ErrLastOwner is returned when a change would leave an owned project with no
// owner. Match with errors.Is.
var ErrLastOwner = errors.New("a project must keep at least one owner")

// ProjectMember is one live member of a project.
type ProjectMember struct {
	ID        string
	ProjectID string
	Member    string // "kind:id"
	Role      string // owner | member | viewer
	CreatedAt string
	CreatedBy sql.NullString
}

const memberColumns = `id, project_id, member, role, created_at, created_by`

func scanMember(row interface{ Scan(...any) error }) (*ProjectMember, error) {
	var m ProjectMember
	if err := row.Scan(&m.ID, &m.ProjectID, &m.Member, &m.Role, &m.CreatedAt, &m.CreatedBy); err != nil {
		return nil, err
	}
	return &m, nil
}

// checkMember validates a member identity and role together: a known role, a
// well-formed identity, and an owner who is a person.
func checkMember(member, role string) (string, error) {
	if !validMemberRoles[role] {
		return "", &FieldError{Field: "role", Value: role, Allowed: memberRoles}
	}
	id, err := ParseIdentity(FieldMember, member)
	if err != nil {
		return "", err
	}
	if role == "owner" {
		if _, err := ParseIdentity(FieldOwner, id); err != nil {
			return "", fmt.Errorf("only a person can be an owner: %w", err)
		}
	}
	return id, nil
}

func (db *DB) creatorColumn(ctx context.Context) (any, sql.NullString) {
	s := db.currentActor(ctx).String()
	if s == "" {
		return nil, sql.NullString{}
	}
	return s, sql.NullString{String: s, Valid: true}
}

// addMemberTx inserts a member unless the identity is already a live member.
// existing is returned (added=false) for the no-op; a different role for an
// existing member is an error that names the role change.
func (db *DB) addMemberTx(ctx context.Context, tx *sql.Tx, projectID, member, role, now string) (*ProjectMember, bool, error) {
	ex, err := scanMember(tx.QueryRowContext(ctx,
		`SELECT `+memberColumns+` FROM project_members WHERE project_id = ? AND member = ? AND deleted_at IS NULL`, projectID, member))
	if err == nil {
		if ex.Role == role {
			return ex, false, nil
		}
		return nil, false, fmt.Errorf("%s is already a member as %s; change the role with SetProjectMemberRole (ledger_set_project_member_role), not by adding again", member, ex.Role)
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}
	by, byNull := db.creatorColumn(ctx)
	m := &ProjectMember{ID: uuid.NewString(), ProjectID: projectID, Member: member, Role: role, CreatedAt: now, CreatedBy: byNull}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO project_members (id, project_id, member, role, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, projectID, member, role, now, by); err != nil {
		return nil, false, fmt.Errorf("adding member: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"project_id": projectID, "member": member, "role": role})
	if err := db.insertAudit(ctx, tx, "project_member", m.ID, "created", string(detail)); err != nil {
		return nil, false, fmt.Errorf("writing audit log: %w", err)
	}
	return m, true, nil
}

// AddProjectMember adds a person or agent to a project with a role. Adding the
// same member with the same role is a no-op (added=false).
func (db *DB) AddProjectMember(ctx context.Context, projectID, member, role string) (*ProjectMember, bool, error) {
	id, err := checkMember(member, role)
	if err != nil {
		return nil, false, err
	}
	if err := db.permit(ctx, OpManageMembers, Target{ProjectID: projectID, EntityType: "project_member"}); err != nil {
		return nil, false, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := refuseIfProjectDeleted(ctx, tx, projectID); err != nil {
		return nil, false, err
	}
	m, added, err := db.addMemberTx(ctx, tx, projectID, id, role, nowUTC())
	if err != nil {
		return nil, false, err
	}
	return m, added, tx.Commit()
}

// liveOwnerCount counts the project's live owners, optionally not counting one row.
func liveOwnerCount(ctx context.Context, tx *sql.Tx, projectID, exceptID string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_members WHERE project_id = ? AND role = 'owner' AND deleted_at IS NULL AND id <> ?`,
		projectID, exceptID).Scan(&n)
	return n, err
}

// RemoveProjectMember removes a member (a soft delete). removed is false if the
// identity was not a member. The last owner cannot be removed.
func (db *DB) RemoveProjectMember(ctx context.Context, projectID, member string) (bool, error) {
	id, err := ParseIdentity(FieldMember, member)
	if err != nil {
		return false, err
	}
	if err := db.permit(ctx, OpManageMembers, Target{ProjectID: projectID, EntityType: "project_member"}); err != nil {
		return false, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := refuseIfProjectDeleted(ctx, tx, projectID); err != nil {
		return false, err
	}
	m, err := scanMember(tx.QueryRowContext(ctx,
		`SELECT `+memberColumns+` FROM project_members WHERE project_id = ? AND member = ? AND deleted_at IS NULL`, projectID, id))
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if m.Role == "owner" {
		if n, err := liveOwnerCount(ctx, tx, projectID, m.ID); err != nil {
			return false, err
		} else if n == 0 {
			return false, fmt.Errorf("%w: %s is the only owner; add another owner first", ErrLastOwner, id)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_members SET deleted_at = ? WHERE id = ?`, nowUTC(), m.ID); err != nil {
		return false, fmt.Errorf("removing member: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"project_id": projectID, "member": id, "role": m.Role})
	if err := db.insertAudit(ctx, tx, "project_member", m.ID, "deleted", string(detail)); err != nil {
		return false, fmt.Errorf("writing audit log: %w", err)
	}
	return true, tx.Commit()
}

// SetProjectMemberRole changes a member's role. The same role writes nothing.
// The last owner cannot be demoted, and only a person can become an owner.
func (db *DB) SetProjectMemberRole(ctx context.Context, projectID, member, role string) (*ProjectMember, error) {
	id, err := checkMember(member, role)
	if err != nil {
		return nil, err
	}
	if err := db.permit(ctx, OpManageMembers, Target{ProjectID: projectID, EntityType: "project_member"}); err != nil {
		return nil, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := refuseIfProjectDeleted(ctx, tx, projectID); err != nil {
		return nil, err
	}
	m, err := scanMember(tx.QueryRowContext(ctx,
		`SELECT `+memberColumns+` FROM project_members WHERE project_id = ? AND member = ? AND deleted_at IS NULL`, projectID, id))
	if err == sql.ErrNoRows {
		return nil, &lookupError{msg: fmt.Sprintf("%s is not a member of this project", id), kind: ErrNotFound}
	}
	if err != nil {
		return nil, err
	}
	if m.Role == role {
		return m, nil
	}
	if m.Role == "owner" {
		if n, err := liveOwnerCount(ctx, tx, projectID, m.ID); err != nil {
			return nil, err
		} else if n == 0 {
			return nil, fmt.Errorf("%w: %s is the only owner; add another owner first", ErrLastOwner, id)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_members SET role = ? WHERE id = ?`, role, m.ID); err != nil {
		return nil, fmt.Errorf("changing role: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"member": id, "from": m.Role, "to": role})
	if err := db.insertAudit(ctx, tx, "project_member", m.ID, "updated", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}
	m.Role = role
	return m, tx.Commit()
}

// ListProjectMembers returns the project's live members: owners first, then
// members, then viewers, each group by name.
func (db *DB) ListProjectMembers(ctx context.Context, projectID string) ([]ProjectMember, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT `+memberColumns+` FROM project_members WHERE project_id = ? AND deleted_at IS NULL
		 ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'member' THEN 1 ELSE 2 END, lower(member)`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectMember
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// refuseLastOwnerRow stops the generic soft delete from removing a project's last
// owner, which RemoveProjectMember already refuses. It runs in the delete's own
// transaction and locks the project first, so the count it makes is not stale by the
// time the delete happens.
func refuseLastOwnerRow(ctx context.Context, tx *sql.Tx, rowID string) error {
	var projectID, role string
	var deleted sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT project_id, role, deleted_at FROM project_members WHERE id = ?`, rowID).Scan(&projectID, &role, &deleted)
	if err == sql.ErrNoRows || deleted.Valid || role != "owner" {
		return nil // not found / already gone / not an owner: the caller's own handling speaks
	}
	if err != nil {
		return err
	}
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE id = ? `+lockDirective, projectID).Scan(&locked); err != nil && err != sql.ErrNoRows {
		return err
	}
	n, err := liveOwnerCount(ctx, tx, projectID, rowID)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: that row is the only owner; add another owner first", ErrLastOwner)
	}
	return nil
}

// isPerson reports whether an identity string is a person.
func isPerson(identity string) bool { return strings.HasPrefix(identity, string(ActorPerson)+":") }
