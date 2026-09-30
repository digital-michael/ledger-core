package ledgercore

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Workflow search, version comparison, pool hygiene and creating tickets for
// gaps -- the parts of the workflow feature built on top of workflows.go.

// SearchWorkflows finds live workflows whose current version mentions query
// anywhere -- title, goal, steps, results, conditions, scope, features,
// constraints. Plain case-insensitive substring matching, like SearchItems.
// projectID "" searches every project. Archived workflows are included:
// search is how an old one is found again.
func (db *DB) SearchWorkflows(ctx context.Context, projectID, query string) ([]Workflow, error) {
	q := `SELECT w.id FROM workflows w
		JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.current_version
		WHERE w.deleted_at IS NULL AND (v.title LIKE '%' || ? || '%' OR v.document LIKE '%' || ? || '%')`
	args := []any{query, query}
	if projectID != "" {
		q += ` AND w.project_id = ?`
		args = append(args, projectID)
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
	out := make([]Workflow, 0, len(ids))
	for _, id := range ids {
		w, err := db.GetWorkflow(ctx, id, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// WorkflowChange is one difference between two versions. Parts are matched
// by key, so a moved step is reported as moved, not as removed-and-added.
type WorkflowChange struct {
	Key    string // s3, s3.r1, f2 ... or title/goal/actor, or parameter:<name>
	Kind   string // field, step, result, alternate, alternate_result, precondition, postcondition, in_scope, out_of_scope, feature, constraint, parameter
	Change string // added, removed, changed, moved
	Moved  bool   // also moved, when Change is "changed"
	Before string
	After  string
}

type partState struct {
	kind, text, parent string
	pos                int
}

// partStates flattens a document into key -> state, with each part's text
// and its position within its own list (so reordering is visible).
func partStates(d *WorkflowDoc) map[string]partState {
	m := map[string]partState{}
	refText := func(r *WorkflowRef) string {
		if r == nil {
			return ""
		}
		if r.Version > 0 {
			return fmt.Sprintf(" [workflow %s v%d]", r.WorkflowID, r.Version)
		}
		return " [workflow " + r.WorkflowID + "]"
	}
	for _, sec := range d.itemSections() {
		for i, it := range *sec.items {
			m[it.Key] = partState{sec.kind, it.Text, sec.kind, i}
		}
	}
	for i, c := range d.Preconditions {
		m[c.Key] = partState{"precondition", c.Text + refText(c.Ref), "preconditions", i}
	}
	for i, st := range d.Steps {
		m[st.Key] = partState{"step", st.Action + refText(st.Ref), "steps", i}
		for j, r := range st.Results {
			m[r.Key] = partState{"result", r.Text, st.Key, j}
		}
	}
	for i, a := range d.AlternatePaths {
		m[a.Key] = partState{"alternate", "at " + a.AtStep + ", if " + a.Condition, "alternates", i}
		for j, r := range a.Results {
			m[r.Key] = partState{"alternate_result", r.Text, a.Key, j}
		}
	}
	for i, c := range d.Postconditions {
		m[c.Key] = partState{"postcondition", c.Text + refText(c.Ref), "postconditions", i}
	}
	for i, p := range d.Parameters {
		text := p.Value
		if p.Description != "" {
			text += " -- " + p.Description
		}
		m["parameter:"+p.Name] = partState{"parameter", text, "parameters", i}
	}
	return m
}

// DiffWorkflowVersions compares version from with version to (0 = current),
// header fields first, then parts in the order they appear in to (removed
// parts last, in from's order).
func (db *DB) DiffWorkflowVersions(ctx context.Context, id string, from, to int) ([]WorkflowChange, error) {
	a, err := db.GetWorkflow(ctx, id, from)
	if err != nil {
		return nil, err
	}
	b, err := db.GetWorkflow(ctx, id, to)
	if err != nil {
		return nil, err
	}
	var out []WorkflowChange
	field := func(key, before, after string) {
		if before != after {
			out = append(out, WorkflowChange{Key: key, Kind: "field", Change: "changed", Before: before, After: after})
		}
	}
	da, dbDoc := a.Version.Doc, b.Version.Doc
	field("title", da.Title, dbDoc.Title)
	field("goal", da.Goal, dbDoc.Goal)
	field("actor", da.Actor, dbDoc.Actor)

	before, after := partStates(&da), partStates(&dbDoc)
	for _, key := range orderedKeys(&dbDoc) {
		st := after[key]
		old, had := before[key]
		switch {
		case !had:
			out = append(out, WorkflowChange{Key: key, Kind: st.kind, Change: "added", After: st.text})
		case old.text != st.text:
			out = append(out, WorkflowChange{Key: key, Kind: st.kind, Change: "changed", Moved: old.pos != st.pos || old.parent != st.parent, Before: old.text, After: st.text})
		case old.pos != st.pos || old.parent != st.parent:
			out = append(out, WorkflowChange{Key: key, Kind: st.kind, Change: "moved", Before: st.text, After: st.text})
		}
	}
	for _, key := range orderedKeys(&da) {
		if _, still := after[key]; !still {
			out = append(out, WorkflowChange{Key: key, Kind: before[key].kind, Change: "removed", Before: before[key].text})
		}
	}
	return out, nil
}

// orderedKeys lists partStates' keys in document order.
func orderedKeys(d *WorkflowDoc) []string {
	var keys []string
	for _, p := range d.keyedParts() {
		keys = append(keys, p.key)
	}
	for _, p := range d.Parameters {
		keys = append(keys, "parameter:"+p.Name)
	}
	return keys
}

// GapTicketResult reports one ticket created (or not) for a gap.
type GapTicketResult struct {
	Key    string
	Title  string
	ItemID string
	Error  error
}

// CreateGapTickets creates one ticket per gap key in the workflow's current
// version, titled from the part's text, and links each as implementing its
// part. keys nil means every current gap. parentID "" leaves the tickets
// unparented. Per-gap, like the other bulk operations: one failure does not
// stop the rest, every result is reported, and the audit rows share a batch
// id so the action reads as one event.
func (db *DB) CreateGapTickets(ctx context.Context, workflowID string, keys []string, parentID, itemType string) ([]GapTicketResult, error) {
	w, err := db.GetWorkflow(ctx, workflowID, 0)
	if err != nil {
		return nil, err
	}
	if w.DeletedAt.Valid {
		return nil, errDeleted("workflow", w.ID)
	}
	prog, err := db.GetWorkflowProgress(ctx, w.ID, 0)
	if err != nil {
		return nil, err
	}
	gaps := map[string]WorkflowGap{}
	var order []string
	for _, g := range prog.Gaps {
		gaps[g.Key] = g
		order = append(order, g.Key)
	}
	if keys == nil {
		keys = order
	}
	if itemType == "" {
		itemType = "task"
	}
	ctx = WithBatch(ctx, uuid.NewString())
	out := make([]GapTicketResult, 0, len(keys))
	for _, key := range keys {
		g, ok := gaps[key]
		if !ok {
			out = append(out, GapTicketResult{Key: key, Error: fmt.Errorf("%s is not a gap in the current version", key)})
			continue
		}
		title := strings.TrimSpace(g.Text)
		if title == "" {
			title = key
		}
		r := GapTicketResult{Key: key, Title: title}
		it, err := db.CreateItem(ctx, CreateItemParams{ProjectID: w.ProjectID, ParentID: parentID, Type: itemType, Title: title,
			Description: fmt.Sprintf("Implements %s of workflow %q (%s).", key, w.Version.Title, w.ID)})
		if err != nil {
			r.Error = err
			out = append(out, r)
			continue
		}
		r.ItemID = it.ID
		if _, err := db.LinkWorkflowTicket(ctx, w.ID, key, it.ID, "implements"); err != nil {
			r.Error = fmt.Errorf("created %s but could not link it: %w", it.ID, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// --- pool hygiene (HealthFindings) --------------------------------------

// maxReadableSteps: past this, a workflow is a candidate for decomposition
// into referenced workflows (Miller's 7 +/- 2). A hint, not an error.
const maxReadableSteps = 9

// projectRestateSteps: a project workflow this long with no references is
// probably restating epic/story workflows. A hint, not an error.
const projectRestateSteps = 4

func (db *DB) checkWorkflows(ctx context.Context) ([]Finding, error) {
	var out []Finding

	// Unassociated: a draft or active workflow no epic or story uses.
	found, err := db.collect(ctx, `
		SELECT w.id, v.title, w.status FROM workflows w
		JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.current_version
		WHERE w.deleted_at IS NULL AND w.status <> 'archived' AND w.project_level = 0
		  AND NOT EXISTS (SELECT 1 FROM workflow_associations a JOIN items i ON i.id = a.item_id
		                  WHERE a.workflow_id = w.id AND a.deleted_at IS NULL AND i.deleted_at IS NULL)
		ORDER BY v.title`,
		func(id, title, status string) Finding {
			return Finding{Check: "workflow_unassociated", EntityType: "workflow", EntityID: id, Title: title,
				Detail: "A " + status + " workflow that no epic or story uses. Associate it, or archive it if it is no longer needed.",
				Fix:    ""}
		})
	if err != nil {
		return nil, err
	}
	out = append(out, found...)

	// Links to deleted tickets.
	found, err = db.collect(ctx, `
		SELECT l.id, i.title, l.part_key FROM workflow_links l
		JOIN workflows w ON w.id = l.workflow_id AND w.deleted_at IS NULL
		JOIN items i ON i.id = l.item_id
		WHERE l.deleted_at IS NULL AND i.deleted_at IS NOT NULL`,
		func(id, title, key string) Finding {
			return Finding{Check: "workflow_link_deleted_ticket", EntityType: "workflow_link", EntityID: id, Title: title,
				Detail: "Linked to " + key + ", but the ticket is deleted, so the part may be a gap again.",
				Fix:    "ledger_delete entity_type=workflow_link id=" + id}
		})
	if err != nil {
		return nil, err
	}
	out = append(out, found...)

	// Per-workflow checks that need the document.
	rows, err := db.conn.QueryContext(ctx, `SELECT id FROM workflows WHERE deleted_at IS NULL`)
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
	for _, id := range ids {
		w, err := db.GetWorkflow(ctx, id, 0)
		if err != nil {
			return nil, err
		}
		doc := &w.Version.Doc
		parts := map[string]bool{}
		for _, p := range doc.keyedParts() {
			parts[p.key] = true
		}
		// Links whose part no longer exists in the current version.
		links, err := db.ListWorkflowLinks(ctx, w.ID, "")
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			if !parts[l.PartKey] {
				out = append(out, Finding{Check: "workflow_link_orphaned", EntityType: "workflow_link", EntityID: l.ID,
					Title:  w.Version.Title + " " + l.PartKey,
					Detail: fmt.Sprintf("A ticket is linked to %s, which the current version (v%d) no longer has.", l.PartKey, w.CurrentVersion),
					Fix:    "ledger_delete entity_type=workflow_link id=" + l.ID})
			}
		}
		// References to archived or deleted workflows.
		for _, r := range doc.refs() {
			var status string
			var deleted bool
			ref, err := db.GetWorkflow(ctx, r.WorkflowID, 0)
			if err != nil {
				status, deleted = "missing", true
			} else {
				status, deleted = ref.Status, ref.DeletedAt.Valid
			}
			if deleted || status == "archived" {
				what := "archived"
				if deleted {
					what = "deleted"
				}
				out = append(out, Finding{Check: "workflow_ref_retired", EntityType: "workflow", EntityID: w.ID, Title: w.Version.Title,
					Detail: "References workflow " + r.WorkflowID + ", which is " + what + ".", Fix: ""})
			}
		}
		// A project workflow should compose epic/story workflows, not restate
		// them: several steps and no references suggests duplication.
		if w.ProjectLevel && len(doc.Steps) >= projectRestateSteps {
			refs := 0
			for _, st := range doc.Steps {
				if st.Ref != nil {
					refs++
				}
			}
			if refs == 0 {
				out = append(out, Finding{Check: "workflow_project_restates", EntityType: "workflow", EntityID: w.ID, Title: w.Version.Title,
					Detail: fmt.Sprintf("A project workflow with %d steps and no references to epic or story workflows. Consider making its steps references, so each behaviour is described once.", len(doc.Steps)),
					Fix:    ""})
			}
		}
		if n := len(doc.Steps); n > maxReadableSteps {
			out = append(out, Finding{Check: "workflow_long", EntityType: "workflow", EntityID: w.ID, Title: w.Version.Title,
				Detail: fmt.Sprintf("%d steps. Past about %d, consider moving a run of steps into its own workflow and referencing it as one step.", n, maxReadableSteps),
				Fix:    ""})
		}
	}
	return out, nil
}
