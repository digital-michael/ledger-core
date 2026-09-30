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

// Workflows: reusable, versioned descriptions of how a goal is achieved,
// associated with epics and stories and linked, part by part, to the tickets
// that implement or verify them. The content model is WorkflowDoc
// (workflow_doc.go); this file is storage and the rules around it.
//
// Rules, all enforced here so every caller gets them:
//   - Every save is a new immutable version; an update names the version it
//     was based on and is refused if the workflow moved since (U11).
//   - Keys are stable across versions and never reused (see WorkflowDoc).
//   - References to other workflows must exist, may not form a loop, and may
//     not newly point at an archived or deleted workflow.
//   - Archived workflows accept no new associations.
//   - Every mutation writes its audit row in the same transaction (U5).

// Workflow is a workflow and one of its versions -- the current one unless a
// specific version was asked for.
type Workflow struct {
	ID             string
	ProjectID      string
	Status         string
	CurrentVersion int
	CreatedBy      sql.NullString
	CreatedAt      string
	UpdatedAt      string
	DeletedAt      sql.NullString
	Version        WorkflowVersion
}

// WorkflowVersion is one saved, immutable version.
type WorkflowVersion struct {
	Number     int
	Title      string
	Doc        WorkflowDoc
	ChangeNote sql.NullString
	CreatedBy  sql.NullString
	CreatedAt  string
}

// WorkflowAssociation ties a workflow to an epic or story, following its
// current version unless PinnedVersion is set.
type WorkflowAssociation struct {
	ID            string
	WorkflowID    string
	ItemID        string
	PinnedVersion sql.NullInt64
	CreatedAt     string
}

// WorkflowLink puts a ticket on one part of a workflow (a step, a result, an
// alternate path, a condition), as implementing it or verifying it.
type WorkflowLink struct {
	ID         string
	WorkflowID string
	PartKey    string
	ItemID     string
	Role       string
	CreatedAt  string
}

// CreateWorkflowParams: Status defaults to draft.
type CreateWorkflowParams struct {
	ProjectID  string
	Doc        WorkflowDoc
	ChangeNote string
	Status     string
}

// WorkflowFilter narrows ListWorkflows. Archived workflows are left out
// unless Status asks for them or IncludeArchived is set.
type WorkflowFilter struct {
	ProjectID       string
	Status          string
	IncludeArchived bool
	// ItemID keeps only workflows associated with that epic or story.
	ItemID string
	// Unassociated keeps only workflows with no live association.
	Unassociated bool
}

// queryer is what *sql.DB and *sql.Tx share for reads.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// CreateWorkflow saves version 1 of a new workflow.
func (db *DB) CreateWorkflow(ctx context.Context, p CreateWorkflowParams) (*Workflow, error) {
	status := p.Status
	if status == "" {
		status = "draft"
	}
	if !validWorkflowStatuses[status] {
		return nil, &FieldError{Field: "status", Value: status, Allowed: workflowStatuses}
	}
	doc := p.Doc
	if err := doc.validateShape(); err != nil {
		return nil, err
	}
	if err := doc.assignKeys(map[string]bool{}); err != nil {
		return nil, err
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := refuseIfProjectDeleted(ctx, tx, p.ProjectID); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	if err := db.checkRefs(ctx, tx, id, &doc, nil); err != nil {
		return nil, err
	}

	now := nowUTC()
	by := nullIfEmpty(db.currentActor(ctx).String())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflows (id, project_id, status, current_version, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?, ?)`, id, p.ProjectID, status, by, now, now); err != nil {
		return nil, fmt.Errorf("inserting workflow: %w", err)
	}
	if err := insertVersion(ctx, tx, id, 1, &doc, p.ChangeNote, by, now); err != nil {
		return nil, err
	}
	detail, _ := json.Marshal(map[string]any{"title": doc.Title, "version": 1, "status": status})
	if err := db.insertAudit(ctx, tx, "workflow", id, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetWorkflow(ctx, id, 0)
}

// UpdateWorkflow saves doc as the next version. baseVersion is the version
// the editor started from; if the workflow has moved on since, nothing is
// saved and a *ConflictError says so -- never resolved silently.
func (db *DB) UpdateWorkflow(ctx context.Context, id string, doc WorkflowDoc, baseVersion int, changeNote string) (*Workflow, error) {
	if err := doc.validateShape(); err != nil {
		return nil, err
	}
	resolved, err := db.resolveWorkflowID(ctx, id)
	if err != nil {
		return nil, err
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var current int
	var deletedAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT current_version, deleted_at FROM workflows WHERE id = ?`, resolved).
		Scan(&current, &deletedAt); err != nil {
		return nil, fmt.Errorf("looking up workflow %q: %w", resolved, err)
	}
	if deletedAt.Valid {
		return nil, errDeleted("workflow", resolved)
	}
	if baseVersion != current {
		return nil, &ConflictError{EntityType: "workflow", EntityID: resolved,
			Expected: fmt.Sprintf("version %d", baseVersion), Actual: fmt.Sprintf("version %d", current)}
	}

	issued, prevRefs, err := issuedKeysAndRefs(ctx, tx, resolved, current)
	if err != nil {
		return nil, err
	}
	if err := doc.assignKeys(issued); err != nil {
		return nil, err
	}
	if err := db.checkRefs(ctx, tx, resolved, &doc, prevRefs); err != nil {
		return nil, err
	}

	next := current + 1
	now := nowUTC()
	by := nullIfEmpty(db.currentActor(ctx).String())
	if err := insertVersion(ctx, tx, resolved, next, &doc, changeNote, by, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflows SET current_version = ?, updated_at = ? WHERE id = ?`,
		next, now, resolved); err != nil {
		return nil, fmt.Errorf("updating workflow: %w", err)
	}
	detail, _ := json.Marshal(map[string]any{"title": doc.Title, "version": next, "change_note": changeNote})
	if err := db.insertAudit(ctx, tx, "workflow", resolved, "updated", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetWorkflow(ctx, resolved, 0)
}

// SetWorkflowStatus moves a workflow between draft, active and archived.
func (db *DB) SetWorkflowStatus(ctx context.Context, id, status string) (*Workflow, error) {
	if !validWorkflowStatuses[status] {
		return nil, &FieldError{Field: "status", Value: status, Allowed: workflowStatuses}
	}
	resolved, err := db.resolveWorkflowID(ctx, id)
	if err != nil {
		return nil, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := refuseIfDeleted(ctx, tx, "workflow", "workflows", resolved); err != nil {
		return nil, err
	}
	var old string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM workflows WHERE id = ?`, resolved).Scan(&old); err != nil {
		return nil, fmt.Errorf("looking up workflow %q: %w", resolved, err)
	}
	if old != status {
		if _, err := tx.ExecContext(ctx, `UPDATE workflows SET status = ?, updated_at = ? WHERE id = ?`,
			status, nowUTC(), resolved); err != nil {
			return nil, fmt.Errorf("updating workflow status: %w", err)
		}
		detail, _ := json.Marshal(map[string]string{"from": old, "to": status})
		if err := db.insertAudit(ctx, tx, "workflow", resolved, "status_changed", string(detail)); err != nil {
			return nil, fmt.Errorf("writing audit log: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetWorkflow(ctx, resolved, 0)
}

// GetWorkflow returns a workflow with its current version, or with version
// n when n > 0. Like GetItem it accepts a unique id prefix and returns a
// soft-deleted workflow (DeletedAt set) for inspection before a restore.
func (db *DB) GetWorkflow(ctx context.Context, id string, version int) (*Workflow, error) {
	resolved, err := db.resolveWorkflowID(ctx, id)
	if err != nil {
		return nil, err
	}
	w := &Workflow{}
	if err := db.conn.QueryRowContext(ctx,
		`SELECT id, project_id, status, current_version, created_by, created_at, updated_at, deleted_at
		 FROM workflows WHERE id = ?`, resolved).
		Scan(&w.ID, &w.ProjectID, &w.Status, &w.CurrentVersion, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt, &w.DeletedAt); err != nil {
		return nil, fmt.Errorf("reading workflow %q: %w", resolved, err)
	}
	if version <= 0 {
		version = w.CurrentVersion
	}
	v, err := loadVersion(ctx, db.conn, w.ID, version)
	if err != nil {
		return nil, err
	}
	w.Version = *v
	return w, nil
}

// ListWorkflows returns workflows with their current versions, most recently
// updated first.
func (db *DB) ListWorkflows(ctx context.Context, f WorkflowFilter) ([]Workflow, error) {
	q := `SELECT w.id FROM workflows w WHERE w.deleted_at IS NULL`
	var args []any
	if f.ProjectID != "" {
		q += ` AND w.project_id = ?`
		args = append(args, f.ProjectID)
	}
	switch {
	case f.Status != "":
		if !validWorkflowStatuses[f.Status] {
			return nil, &FieldError{Field: "status", Value: f.Status, Allowed: workflowStatuses}
		}
		q += ` AND w.status = ?`
		args = append(args, f.Status)
	case !f.IncludeArchived:
		q += ` AND w.status <> 'archived'`
	}
	if f.ItemID != "" {
		q += ` AND EXISTS (SELECT 1 FROM workflow_associations a WHERE a.workflow_id = w.id AND a.item_id = ? AND a.deleted_at IS NULL)`
		args = append(args, f.ItemID)
	}
	if f.Unassociated {
		q += ` AND NOT EXISTS (SELECT 1 FROM workflow_associations a JOIN items i ON i.id = a.item_id
		        WHERE a.workflow_id = w.id AND a.deleted_at IS NULL AND i.deleted_at IS NULL)`
	}
	q += ` ORDER BY w.updated_at DESC`

	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Workflow, 0, len(ids))
	for _, id := range ids {
		w, err := db.GetWorkflow(ctx, id, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, nil
}

// ListWorkflowVersions returns every version, oldest first.
func (db *DB) ListWorkflowVersions(ctx context.Context, id string) ([]WorkflowVersion, error) {
	resolved, err := db.resolveWorkflowID(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := db.conn.QueryContext(ctx,
		`SELECT version FROM workflow_versions WHERE workflow_id = ? ORDER BY version`, resolved)
	if err != nil {
		return nil, err
	}
	var numbers []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		numbers = append(numbers, n)
	}
	rows.Close()
	out := make([]WorkflowVersion, 0, len(numbers))
	for _, n := range numbers {
		v, err := loadVersion(ctx, db.conn, resolved, n)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// AssociateWorkflow ties a workflow to an epic or story. pinnedVersion 0
// follows the current version.
func (db *DB) AssociateWorkflow(ctx context.Context, workflowID, itemID string, pinnedVersion int) (*WorkflowAssociation, error) {
	wid, err := db.resolveWorkflowID(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	iid, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return nil, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := refuseIfItemDeleted(ctx, tx, iid); err != nil {
		return nil, err
	}
	status, current, err := liveWorkflow(ctx, tx, wid)
	if err != nil {
		return nil, err
	}
	if status == "archived" {
		return nil, docError("workflow %s is archived and accepts no new associations; set it active first", wid)
	}
	if pinnedVersion < 0 || pinnedVersion > current {
		return nil, docError("workflow %s has no version %d (current is %d)", wid, pinnedVersion, current)
	}
	var itemType string
	if err := tx.QueryRowContext(ctx, `SELECT type FROM items WHERE id = ?`, iid).Scan(&itemType); err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", iid, err)
	}
	if !validWorkflowItemTypes[itemType] {
		return nil, &FieldError{Field: "item type", Value: itemType, Allowed: workflowItemTypes}
	}
	var dup string
	err = tx.QueryRowContext(ctx, `SELECT id FROM workflow_associations
		WHERE workflow_id = ? AND item_id = ? AND deleted_at IS NULL`, wid, iid).Scan(&dup)
	if err == nil {
		return nil, docError("workflow %s is already associated with %s (association %s)", wid, iid, dup)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	a := &WorkflowAssociation{ID: uuid.NewString(), WorkflowID: wid, ItemID: iid, CreatedAt: nowUTC()}
	if pinnedVersion > 0 {
		a.PinnedVersion = sql.NullInt64{Int64: int64(pinnedVersion), Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_associations (id, workflow_id, item_id, pinned_version, created_at)
		VALUES (?, ?, ?, ?, ?)`, a.ID, wid, iid, a.PinnedVersion, a.CreatedAt); err != nil {
		return nil, fmt.Errorf("inserting association: %w", err)
	}
	detail, _ := json.Marshal(map[string]any{"workflow_id": wid, "item_id": iid, "pinned_version": pinnedVersion})
	if err := db.insertAudit(ctx, tx, "workflow_association", a.ID, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}
	return a, tx.Commit()
}

// DisassociateWorkflow soft-deletes the live association between a workflow
// and an item; ledger_restore brings it back.
func (db *DB) DisassociateWorkflow(ctx context.Context, workflowID, itemID string) error {
	wid, err := db.resolveWorkflowID(ctx, workflowID)
	if err != nil {
		return err
	}
	iid, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return err
	}
	var id string
	err = db.conn.QueryRowContext(ctx, `SELECT id FROM workflow_associations
		WHERE workflow_id = ? AND item_id = ? AND deleted_at IS NULL`, wid, iid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return &lookupError{msg: fmt.Sprintf("workflow %s is not associated with %s", wid, iid), kind: ErrNotFound}
	}
	if err != nil {
		return err
	}
	return db.SoftDelete(ctx, "workflow_association", id)
}

// ListWorkflowAssociations returns a workflow's live associations.
func (db *DB) ListWorkflowAssociations(ctx context.Context, workflowID string) ([]WorkflowAssociation, error) {
	wid, err := db.resolveWorkflowID(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT id, workflow_id, item_id, pinned_version, created_at
		FROM workflow_associations WHERE workflow_id = ? AND deleted_at IS NULL ORDER BY created_at`, wid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowAssociation
	for rows.Next() {
		var a WorkflowAssociation
		if err := rows.Scan(&a.ID, &a.WorkflowID, &a.ItemID, &a.PinnedVersion, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LinkWorkflowTicket puts a ticket on one part of a workflow's current
// version, as implementing or verifying it.
func (db *DB) LinkWorkflowTicket(ctx context.Context, workflowID, partKey, itemID, role string) (*WorkflowLink, error) {
	if !validWorkflowLinkRoles[role] {
		return nil, &FieldError{Field: "role", Value: role, Allowed: workflowLinkRoles}
	}
	wid, err := db.resolveWorkflowID(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	iid, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return nil, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := refuseIfItemDeleted(ctx, tx, iid); err != nil {
		return nil, err
	}
	_, current, err := liveWorkflow(ctx, tx, wid)
	if err != nil {
		return nil, err
	}
	v, err := loadVersion(ctx, tx, wid, current)
	if err != nil {
		return nil, err
	}
	known := false
	for _, p := range v.Doc.keyedParts() {
		if p.key == partKey {
			known = true
			break
		}
	}
	if !known {
		return nil, docError("workflow %s version %d has no part %q", wid, current, partKey)
	}
	var dup string
	err = tx.QueryRowContext(ctx, `SELECT id FROM workflow_links WHERE workflow_id = ? AND part_key = ?
		AND item_id = ? AND role = ? AND deleted_at IS NULL`, wid, partKey, iid, role).Scan(&dup)
	if err == nil {
		return nil, docError("ticket %s already %s %s (link %s)", iid, role, partKey, dup)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	l := &WorkflowLink{ID: uuid.NewString(), WorkflowID: wid, PartKey: partKey, ItemID: iid, Role: role, CreatedAt: nowUTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_links (id, workflow_id, part_key, item_id, role, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, l.ID, wid, partKey, iid, role, l.CreatedAt); err != nil {
		return nil, fmt.Errorf("inserting link: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"workflow_id": wid, "part_key": partKey, "item_id": iid, "role": role})
	if err := db.insertAudit(ctx, tx, "workflow_link", l.ID, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}
	return l, tx.Commit()
}

// UnlinkWorkflowTicket soft-deletes a live link; ledger_restore brings it back.
func (db *DB) UnlinkWorkflowTicket(ctx context.Context, workflowID, partKey, itemID, role string) error {
	wid, err := db.resolveWorkflowID(ctx, workflowID)
	if err != nil {
		return err
	}
	iid, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return err
	}
	var id string
	err = db.conn.QueryRowContext(ctx, `SELECT id FROM workflow_links WHERE workflow_id = ? AND part_key = ?
		AND item_id = ? AND role = ? AND deleted_at IS NULL`, wid, partKey, iid, role).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return &lookupError{msg: fmt.Sprintf("no %s link from %s to %s on workflow %s", role, iid, partKey, wid), kind: ErrNotFound}
	}
	if err != nil {
		return err
	}
	return db.SoftDelete(ctx, "workflow_link", id)
}

// ListWorkflowLinks returns a workflow's live ticket links. Pass a workflow
// id, or an empty workflowID with an itemID for "which workflow parts does
// this ticket sit on".
func (db *DB) ListWorkflowLinks(ctx context.Context, workflowID, itemID string) ([]WorkflowLink, error) {
	q := `SELECT id, workflow_id, part_key, item_id, role, created_at FROM workflow_links WHERE deleted_at IS NULL`
	var args []any
	if workflowID != "" {
		wid, err := db.resolveWorkflowID(ctx, workflowID)
		if err != nil {
			return nil, err
		}
		q += ` AND workflow_id = ?`
		args = append(args, wid)
	}
	if itemID != "" {
		iid, err := db.resolveItemID(ctx, itemID)
		if err != nil {
			return nil, err
		}
		q += ` AND item_id = ?`
		args = append(args, iid)
	}
	if len(args) == 0 {
		return nil, errors.New("ListWorkflowLinks: give a workflow id, an item id, or both")
	}
	q += ` ORDER BY created_at`
	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowLink
	for rows.Next() {
		var l WorkflowLink
		if err := rows.Scan(&l.ID, &l.WorkflowID, &l.PartKey, &l.ItemID, &l.Role, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// WorkflowGap is a part of a workflow that no live ticket implements.
type WorkflowGap struct {
	Key  string // s3, s3.r1, a1 ...
	Kind string // step, result, alternate, alternate_result, feature
	Text string
}

// WorkflowProgress summarises a workflow version against its tickets.
//
// A step is covered when a live ticket implements the step itself, or --
// when nothing is on the step -- every one of its results; a step that
// references another workflow is covered by that workflow. A covered step
// is ready when every ticket implementing it (or its results) is done, or,
// for a reference, when the referenced workflow is fully ready. Alternate
// paths produce gaps but do not count as steps.
type WorkflowProgress struct {
	Version int
	Steps   int
	Covered int
	Ready   int
	Gaps    []WorkflowGap
}

// GetWorkflowProgress computes progress for version n (0 = current).
func (db *DB) GetWorkflowProgress(ctx context.Context, id string, version int) (*WorkflowProgress, error) {
	resolved, err := db.resolveWorkflowID(ctx, id)
	if err != nil {
		return nil, err
	}
	return db.progress(ctx, resolved, version, map[string]bool{})
}

func (db *DB) progress(ctx context.Context, wid string, version int, visiting map[string]bool) (*WorkflowProgress, error) {
	if visiting[wid] {
		// Loops are refused on write; this only guards data written around
		// this package.
		return nil, fmt.Errorf("workflow reference loop through %s", wid)
	}
	visiting[wid] = true
	defer delete(visiting, wid)

	w, err := db.GetWorkflow(ctx, wid, version)
	if err != nil {
		return nil, err
	}
	// Implementing tickets per part, with whether each is done. Deleted
	// tickets implement nothing.
	rows, err := db.conn.QueryContext(ctx, `SELECT l.part_key, i.status FROM workflow_links l
		JOIN items i ON i.id = l.item_id
		WHERE l.workflow_id = ? AND l.role = 'implements' AND l.deleted_at IS NULL AND i.deleted_at IS NULL`, wid)
	if err != nil {
		return nil, err
	}
	type tickets struct{ n, done int }
	impl := map[string]*tickets{}
	for rows.Next() {
		var key, status string
		if err := rows.Scan(&key, &status); err != nil {
			rows.Close()
			return nil, err
		}
		t := impl[key]
		if t == nil {
			t = &tickets{}
			impl[key] = t
		}
		t.n++
		if status == "done" {
			t.done++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	p := &WorkflowProgress{Version: w.Version.Number, Gaps: []WorkflowGap{}}
	// covered reports whether a step-like part (key plus results) is covered,
	// whether all its tickets are done, and the gaps it contributes.
	covered := func(key, kind, text string, results []WorkflowResult, resultKind string) (bool, bool, []WorkflowGap) {
		if t := impl[key]; t != nil {
			allDone := t.done == t.n
			for _, r := range results {
				if rt := impl[r.Key]; rt != nil && rt.done < rt.n {
					allDone = false
				}
			}
			return true, allDone, nil
		}
		var gaps []WorkflowGap
		linked, allDone := false, true
		for _, r := range results {
			rt := impl[r.Key]
			if rt == nil {
				gaps = append(gaps, WorkflowGap{Key: r.Key, Kind: resultKind, Text: r.Text})
				continue
			}
			linked = true
			if rt.done < rt.n {
				allDone = false
			}
		}
		if !linked {
			return false, false, []WorkflowGap{{Key: key, Kind: kind, Text: text}}
		}
		return len(gaps) == 0, len(gaps) == 0 && allDone, gaps
	}

	for _, s := range w.Version.Doc.Steps {
		p.Steps++
		if s.Ref != nil {
			p.Covered++
			sub, err := db.progress(ctx, s.Ref.WorkflowID, s.Ref.Version, visiting)
			if err != nil {
				return nil, err
			}
			if sub.Steps > 0 && sub.Ready == sub.Steps {
				p.Ready++
			}
			continue
		}
		ok, ready, gaps := covered(s.Key, "step", s.Action, s.Results, "result")
		p.Gaps = append(p.Gaps, gaps...)
		if ok {
			p.Covered++
		}
		if ready {
			p.Ready++
		}
	}
	for _, a := range w.Version.Doc.AlternatePaths {
		_, _, gaps := covered(a.Key, "alternate", a.Condition, a.Results, "alternate_result")
		p.Gaps = append(p.Gaps, gaps...)
	}
	// Features need building, so an unimplemented one is a gap. Scope and
	// constraints are checked, not built, and never are.
	for _, f := range w.Version.Doc.Features {
		if impl[f.Key] == nil {
			p.Gaps = append(p.Gaps, WorkflowGap{Key: f.Key, Kind: "feature", Text: f.Text})
		}
	}
	return p, nil
}

// --- internals ---------------------------------------------------------

func insertVersion(ctx context.Context, tx *sql.Tx, wid string, n int, doc *WorkflowDoc, note string, by any, now string) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encoding workflow document: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_versions
		(id, workflow_id, version, title, document, change_note, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), wid, n, doc.Title, string(body), nullIfEmpty(note), by, now); err != nil {
		return fmt.Errorf("inserting workflow version: %w", err)
	}
	return nil
}

func loadVersion(ctx context.Context, q queryer, wid string, n int) (*WorkflowVersion, error) {
	v := &WorkflowVersion{Number: n}
	var body string
	err := q.QueryRowContext(ctx, `SELECT title, document, change_note, created_by, created_at
		FROM workflow_versions WHERE workflow_id = ? AND version = ?`, wid, n).
		Scan(&v.Title, &body, &v.ChangeNote, &v.CreatedBy, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &lookupError{msg: fmt.Sprintf("workflow %s has no version %d", wid, n), kind: ErrNotFound}
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(body), &v.Doc); err != nil {
		return nil, fmt.Errorf("decoding workflow %s version %d: %w", wid, n, err)
	}
	return v, nil
}

// liveWorkflow returns status and current version of a workflow that is not
// deleted.
func liveWorkflow(ctx context.Context, tx *sql.Tx, wid string) (string, int, error) {
	var status string
	var current int
	var deletedAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, current_version, deleted_at FROM workflows WHERE id = ?`, wid).
		Scan(&status, &current, &deletedAt); err != nil {
		return "", 0, fmt.Errorf("looking up workflow %q: %w", wid, err)
	}
	if deletedAt.Valid {
		return "", 0, errDeleted("workflow", wid)
	}
	return status, current, nil
}

// issuedKeysAndRefs collects every key any version of the workflow ever
// used (so keys are never reused), and the references in the current
// version (which stay allowed even if their target was archived since).
func issuedKeysAndRefs(ctx context.Context, tx *sql.Tx, wid string, current int) (map[string]bool, map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version FROM workflow_versions WHERE workflow_id = ?`, wid)
	if err != nil {
		return nil, nil, err
	}
	var versions []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		versions = append(versions, n)
	}
	rows.Close()
	issued, prevRefs := map[string]bool{}, map[string]bool{}
	for _, n := range versions {
		v, err := loadVersion(ctx, tx, wid, n)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range v.Doc.keyedParts() {
			issued[p.key] = true
		}
		if n == current {
			for _, r := range v.Doc.refs() {
				prevRefs[r.WorkflowID] = true
			}
		}
	}
	return issued, prevRefs, nil
}

// checkRefs resolves every reference in doc to a full workflow id (in
// place), and refuses one that is missing, deleted, newly archived, points
// at a version that does not exist, or would close a loop back to self.
func (db *DB) checkRefs(ctx context.Context, tx *sql.Tx, self string, doc *WorkflowDoc, prevRefs map[string]bool) error {
	resolveRef := func(r *WorkflowRef) error {
		full, err := db.resolveWorkflowIDTx(ctx, tx, r.WorkflowID)
		if err != nil {
			return err
		}
		r.WorkflowID = full
		if full == self {
			return docError("a workflow cannot reference itself")
		}
		status, current, err := liveWorkflow(ctx, tx, full)
		if err != nil {
			return err
		}
		if status == "archived" && !prevRefs[full] {
			return docError("workflow %s is archived; new references to it are refused", full)
		}
		if r.Version > current {
			return docError("workflow %s has no version %d (current is %d)", full, r.Version, current)
		}
		if path := findLoop(ctx, tx, full, r.Version, self, nil); path != nil {
			return docError("reference to %s would create a loop: %s", full, strings.Join(append([]string{self}, path...), " -> "))
		}
		return nil
	}
	for i := range doc.Preconditions {
		if doc.Preconditions[i].Ref != nil {
			if err := resolveRef(doc.Preconditions[i].Ref); err != nil {
				return err
			}
		}
	}
	for i := range doc.Steps {
		if doc.Steps[i].Ref != nil {
			if err := resolveRef(doc.Steps[i].Ref); err != nil {
				return err
			}
		}
	}
	for i := range doc.Postconditions {
		if doc.Postconditions[i].Ref != nil {
			if err := resolveRef(doc.Postconditions[i].Ref); err != nil {
				return err
			}
		}
	}
	return nil
}

// findLoop walks references from workflow wid (at version, 0 = current) and
// returns the path to target if one exists. seen stops revisiting.
func findLoop(ctx context.Context, tx *sql.Tx, wid string, version int, target string, seen map[string]bool) []string {
	if seen == nil {
		seen = map[string]bool{}
	}
	if seen[wid] {
		return nil
	}
	seen[wid] = true
	if version <= 0 {
		if err := tx.QueryRowContext(ctx, `SELECT current_version FROM workflows WHERE id = ?`, wid).Scan(&version); err != nil {
			return nil
		}
	}
	v, err := loadVersion(ctx, tx, wid, version)
	if err != nil {
		return nil
	}
	for _, r := range v.Doc.refs() {
		if r.WorkflowID == target {
			return []string{wid, target}
		}
		if path := findLoop(ctx, tx, r.WorkflowID, r.Version, target, seen); path != nil {
			return append([]string{wid}, path...)
		}
	}
	return nil
}

func (db *DB) resolveWorkflowID(ctx context.Context, id string) (string, error) {
	return resolveWorkflowIDWith(ctx, db.conn, id)
}

func (db *DB) resolveWorkflowIDTx(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	return resolveWorkflowIDWith(ctx, tx, id)
}

// resolveWorkflowIDWith is resolveItemID for workflows: an exact id, else a
// unique prefix; an ambiguous prefix lists every candidate.
func resolveWorkflowIDWith(ctx context.Context, q queryer, id string) (string, error) {
	if id == "" {
		return "", &lookupError{msg: "workflow id is required", kind: ErrNotFound}
	}
	rows, err := q.QueryContext(ctx, `SELECT w.id, COALESCE(v.title, '') FROM workflows w
		LEFT JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.current_version
		WHERE w.id = ? OR w.id LIKE ? || '%'`, id, id)
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
		if c.id == id {
			return c.id, nil
		}
		matches = append(matches, c)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", &lookupError{msg: fmt.Sprintf("workflow %q not found", id), kind: ErrNotFound}
	case 1:
		return matches[0].id, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "id %q matches more than one workflow, be more specific:\n", id)
	for _, m := range matches {
		fmt.Fprintf(&sb, "  %s %q\n", m.id, m.title)
	}
	return "", &lookupError{msg: strings.TrimRight(sb.String(), "\n"), kind: ErrAmbiguousID}
}
