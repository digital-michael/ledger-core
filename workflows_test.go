package ledgercore

import (
	"errors"
	"strings"
	"testing"
)

// Workflow tests run against a real SQLite ledger, like the rest of this
// package (see ledgercore_test.go).

func mustTyped(t *testing.T, db *DB, projectID, typ, title string) *Item {
	t.Helper()
	it, err := db.CreateItem(ctx, CreateItemParams{ProjectID: projectID, Type: typ, Title: title})
	if err != nil {
		t.Fatalf("CreateItem %s: %v", typ, err)
	}
	return it
}

// sampleDoc is the "Admin creates a public project" sample from the
// workflows epic, trimmed to what the tests need.
func sampleDoc() WorkflowDoc {
	return WorkflowDoc{
		Title: "Admin creates a public project",
		Goal:  "A new project exists, is public, and anyone can view it read-only.",
		Actor: "ledger admin",
		Parameters: []WorkflowParam{
			{Name: "base_url", Value: "http://127.0.0.1:8090"},
			{Name: "project_name", Value: "test-public"},
		},
		Preconditions: []WorkflowCondition{{Text: "ledger-server is reachable at {base_url}"}},
		Steps: []WorkflowStep{
			{Action: "Open {base_url}", Results: []WorkflowResult{{Text: "Projects page shows a New project button"}}},
			{Action: "Click New project", Results: []WorkflowResult{{Text: "the new-project form opens"}}},
			{Action: "Click Save", Results: []WorkflowResult{{Text: "the form is validated"}, {Text: "the actor is listed as owner"}}},
		},
		AlternatePaths: []WorkflowAltPath{{AtStep: "3", Condition: "name already taken", Results: []WorkflowResult{{Text: "Save is refused"}}}},
		Postconditions: []WorkflowCondition{{Text: "{project_name} is listed as public"}},
	}
}

func mustWorkflow(t *testing.T, db *DB, projectID string, doc WorkflowDoc) *Workflow {
	t.Helper()
	w, err := db.CreateWorkflow(ctx, CreateWorkflowParams{ProjectID: projectID, Doc: doc})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	return w
}

func TestCreateWorkflowAssignsKeysAndRecords(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	w := mustWorkflow(t, db, p.ID, sampleDoc())

	if w.Status != "draft" || w.CurrentVersion != 1 || w.Version.Number != 1 {
		t.Errorf("new workflow: status=%s current=%d version=%d", w.Status, w.CurrentVersion, w.Version.Number)
	}
	if w.CreatedBy.String != "program:"+testClient || w.Version.CreatedBy.String != "program:"+testClient {
		t.Errorf("created_by: workflow=%q version=%q", w.CreatedBy.String, w.Version.CreatedBy.String)
	}
	d := w.Version.Doc
	var keys []string
	for _, part := range d.keyedParts() {
		keys = append(keys, part.key)
	}
	want := "p1 s1 s1.r1 s2 s2.r1 s3 s3.r1 s3.r2 a1 a1.r1 q1"
	if got := strings.Join(keys, " "); got != want {
		t.Errorf("keys:\n got %s\nwant %s", got, want)
	}
	if d.AlternatePaths[0].AtStep != "s3" {
		t.Errorf("at_step position should be stored as a key, got %q", d.AlternatePaths[0].AtStep)
	}
	audit, _ := db.ListAuditLog(ctx, AuditFilter{EntityType: "workflow", EntityID: w.ID})
	if len(audit) != 1 || audit[0].Operation != "created" {
		t.Errorf("audit: %+v", audit)
	}
}

func TestWorkflowDocumentValidation(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	cases := []struct {
		name   string
		mutate func(*WorkflowDoc)
		want   string
	}{
		{"no title", func(d *WorkflowDoc) { d.Title = " " }, "title is required"},
		{"no goal", func(d *WorkflowDoc) { d.Goal = "" }, "goal is required"},
		{"no steps", func(d *WorkflowDoc) { d.Steps = nil }, "at least one step"},
		{"bad param name", func(d *WorkflowDoc) { d.Parameters[0].Name = "Base-URL" }, "lowercase letters"},
		{"duplicate param", func(d *WorkflowDoc) { d.Parameters[1].Name = "base_url" }, "defined twice"},
		{"empty step", func(d *WorkflowDoc) { d.Steps[1].Action = "" }, "needs an action or a workflow reference"},
		{"empty result", func(d *WorkflowDoc) { d.Steps[0].Results[0].Text = "" }, "text is required"},
		{"alt at missing step", func(d *WorkflowDoc) { d.AlternatePaths[0].AtStep = "9" }, "is not a step"},
		{"alt at unknown key", func(d *WorkflowDoc) { d.AlternatePaths[0].AtStep = "s9" }, "is not a step"},
		{"invented key", func(d *WorkflowDoc) { d.Steps[0].Key = "s7" }, "never issued"},
		{"condition without text", func(d *WorkflowDoc) { d.Preconditions[0].Text = "" }, "needs text or a workflow reference"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := sampleDoc()
			c.mutate(&d)
			_, err := db.CreateWorkflow(ctx, CreateWorkflowParams{ProjectID: p.ID, Doc: d})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
			if !errors.Is(err, ErrInvalidField) {
				t.Errorf("should classify as ErrInvalidField: %v", err)
			}
		})
	}
	if _, err := db.CreateWorkflow(ctx, CreateWorkflowParams{ProjectID: p.ID, Doc: sampleDoc(), Status: "live"}); !errors.Is(err, ErrInvalidField) {
		t.Errorf("invalid status: %v", err)
	}
}

func TestUpdateWorkflowVersionsKeysAndConflicts(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	w := mustWorkflow(t, db, p.ID, sampleDoc())

	// A stale base version is refused and saves nothing.
	d := w.Version.Doc
	if _, err := db.UpdateWorkflow(ctx, w.ID, d, 0, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale base should conflict: %v", err)
	}

	// Remove step 2, insert a new step first: keys of the kept steps survive,
	// the new step gets a fresh key, and s2 is never handed out again.
	d.Steps = []WorkflowStep{{Action: "Sign in"}, d.Steps[0], d.Steps[2]}
	w2, err := db.UpdateWorkflow(ctx, w.ID[:8], d, 1, "drop step 2, add sign-in")
	if err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}
	if w2.CurrentVersion != 2 || w2.Version.ChangeNote.String != "drop step 2, add sign-in" {
		t.Errorf("version 2: %+v", w2.Version)
	}
	var got []string
	for _, s := range w2.Version.Doc.Steps {
		got = append(got, s.Key)
	}
	if strings.Join(got, " ") != "s4 s1 s3" {
		t.Errorf("step keys after edit: %v (want s4 s1 s3)", got)
	}

	// Re-adding a step later still does not reuse s2.
	d2 := w2.Version.Doc
	d2.Steps = append(d2.Steps, WorkflowStep{Action: "Close the tab"})
	w3, err := db.UpdateWorkflow(ctx, w.ID, d2, 2, "")
	if err != nil {
		t.Fatalf("third version: %v", err)
	}
	if k := w3.Version.Doc.Steps[3].Key; k != "s5" {
		t.Errorf("new step key %q, want s5 (s2 retired)", k)
	}

	// A result moved under another step is refused.
	d3 := w3.Version.Doc
	d3.Steps[1].Results = append(d3.Steps[1].Results, WorkflowResult{Key: "s3.r1", Text: "moved"})
	if _, err := db.UpdateWorkflow(ctx, w.ID, d3, 3, ""); err == nil || !strings.Contains(err.Error(), "used twice") && !strings.Contains(err.Error(), "belongs to another step") {
		t.Errorf("moved result key: %v", err)
	}

	// Old versions stay readable and unchanged.
	v1, err := db.GetWorkflow(ctx, w.ID, 1)
	if err != nil || len(v1.Version.Doc.Steps) != 3 || v1.Version.Doc.Steps[1].Key != "s2" {
		t.Fatalf("version 1 read back: %v %+v", err, v1)
	}
	versions, _ := db.ListWorkflowVersions(ctx, w.ID)
	if len(versions) != 3 {
		t.Errorf("versions: %d", len(versions))
	}
}

func TestWorkflowReferences(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	signIn := mustWorkflow(t, db, p.ID, WorkflowDoc{Title: "Admin signs in", Goal: "signed in", Steps: []WorkflowStep{{Action: "sign in"}}})

	d := sampleDoc()
	d.Preconditions = append(d.Preconditions, WorkflowCondition{Ref: &WorkflowRef{WorkflowID: signIn.ID[:8]}})
	main := mustWorkflow(t, db, p.ID, d)
	if got := main.Version.Doc.Preconditions[1].Ref.WorkflowID; got != signIn.ID {
		t.Errorf("reference prefix should be stored as the full id, got %q", got)
	}

	// Closing the loop from the other side is refused.
	back := signIn.Version.Doc
	back.Steps = append(back.Steps, WorkflowStep{Ref: &WorkflowRef{WorkflowID: main.ID}})
	if _, err := db.UpdateWorkflow(ctx, signIn.ID, back, 1, ""); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Errorf("loop should be refused: %v", err)
	}
	self := main.Version.Doc
	self.Steps = append(self.Steps, WorkflowStep{Ref: &WorkflowRef{WorkflowID: main.ID}})
	if _, err := db.UpdateWorkflow(ctx, main.ID, self, 1, ""); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Errorf("self reference: %v", err)
	}

	// A pinned version must exist.
	pinned := sampleDoc()
	pinned.Steps = append(pinned.Steps, WorkflowStep{Ref: &WorkflowRef{WorkflowID: signIn.ID, Version: 5}})
	if _, err := db.CreateWorkflow(ctx, CreateWorkflowParams{ProjectID: p.ID, Doc: pinned}); err == nil || !strings.Contains(err.Error(), "no version 5") {
		t.Errorf("missing pinned version: %v", err)
	}

	// Archived: existing references survive an edit, new ones are refused.
	if _, err := db.SetWorkflowStatus(ctx, signIn.ID, "archived"); err != nil {
		t.Fatal(err)
	}
	keep := main.Version.Doc
	keep.Goal = "edited"
	if _, err := db.UpdateWorkflow(ctx, main.ID, keep, 1, "keeps its reference"); err != nil {
		t.Errorf("existing reference to an archived workflow should survive an edit: %v", err)
	}
	fresh := sampleDoc()
	fresh.Steps = append(fresh.Steps, WorkflowStep{Ref: &WorkflowRef{WorkflowID: signIn.ID}})
	if _, err := db.CreateWorkflow(ctx, CreateWorkflowParams{ProjectID: p.ID, Doc: fresh}); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Errorf("new reference to archived: %v", err)
	}
}

func TestWorkflowStatusAssociationsAndListing(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	epic := mustTyped(t, db, p.ID, "epic", "Epic")
	story := mustTyped(t, db, p.ID, "story", "Story")
	task := mustTyped(t, db, p.ID, "task", "Task")
	w := mustWorkflow(t, db, p.ID, sampleDoc())
	loose := mustWorkflow(t, db, p.ID, WorkflowDoc{Title: "Loose", Goal: "g", Steps: []WorkflowStep{{Action: "a"}}})

	if _, err := db.AssociateWorkflow(ctx, w.ID, task.ID, 0); !errors.Is(err, ErrInvalidField) {
		t.Errorf("a task is not associable: %v", err)
	}
	if _, err := db.AssociateWorkflow(ctx, w.ID, epic.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AssociateWorkflow(ctx, w.ID, epic.ID, 0); err == nil {
		t.Error("duplicate association should be refused")
	}
	if _, err := db.AssociateWorkflow(ctx, w.ID, story.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AssociateWorkflow(ctx, w.ID, story.ID, 9); err == nil {
		t.Error("pin to a missing version should be refused")
	}

	ofEpic, _ := db.ListWorkflows(ctx, WorkflowFilter{ItemID: epic.ID})
	if len(ofEpic) != 1 || ofEpic[0].ID != w.ID {
		t.Errorf("by item: %+v", ofEpic)
	}
	unassoc, _ := db.ListWorkflows(ctx, WorkflowFilter{ProjectID: p.ID, Unassociated: true})
	if len(unassoc) != 1 || unassoc[0].ID != loose.ID {
		t.Errorf("unassociated: %+v", unassoc)
	}

	// Disassociate, then restore through the generic path.
	if err := db.DisassociateWorkflow(ctx, w.ID, epic.ID); err != nil {
		t.Fatal(err)
	}
	assocs, _ := db.ListWorkflowAssociations(ctx, w.ID)
	if len(assocs) != 1 || assocs[0].ItemID != story.ID || !assocs[0].PinnedVersion.Valid {
		t.Errorf("after disassociate: %+v", assocs)
	}

	// Archived: hidden by default, listed on request, closed to associations.
	if _, err := db.SetWorkflowStatus(ctx, loose.ID, "archived"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetWorkflowStatus(ctx, loose.ID, "retired"); !errors.Is(err, ErrInvalidField) {
		t.Errorf("invalid status: %v", err)
	}
	all, _ := db.ListWorkflows(ctx, WorkflowFilter{ProjectID: p.ID})
	withArchived, _ := db.ListWorkflows(ctx, WorkflowFilter{ProjectID: p.ID, IncludeArchived: true})
	if len(all) != 1 || len(withArchived) != 2 {
		t.Errorf("listing: default=%d includeArchived=%d", len(all), len(withArchived))
	}
	if _, err := db.AssociateWorkflow(ctx, loose.ID, epic.ID, 0); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Errorf("archived association: %v", err)
	}
}

func TestWorkflowLinksAndProgress(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	w := mustWorkflow(t, db, p.ID, sampleDoc())
	t1 := mustTyped(t, db, p.ID, "task", "build step 1")
	t3 := mustTyped(t, db, p.ID, "task", "owner shown")
	test := mustTyped(t, db, p.ID, "task", "test step 1")

	if _, err := db.LinkWorkflowTicket(ctx, w.ID, "s9", t1.ID, "implements"); err == nil || !strings.Contains(err.Error(), "no part") {
		t.Errorf("unknown part: %v", err)
	}
	if _, err := db.LinkWorkflowTicket(ctx, w.ID, "s1", t1.ID, "owns"); !errors.Is(err, ErrInvalidField) {
		t.Errorf("invalid role: %v", err)
	}
	for _, l := range []struct{ key, item, role string }{
		{"s1", t1.ID, "implements"},
		{"s1", test.ID, "verifies"},
		{"s3.r2", t3.ID, "implements"},
	} {
		if _, err := db.LinkWorkflowTicket(ctx, w.ID, l.key, l.item, l.role); err != nil {
			t.Fatalf("link %v: %v", l, err)
		}
	}
	if _, err := db.LinkWorkflowTicket(ctx, w.ID, "s1", t1.ID, "implements"); err == nil {
		t.Error("duplicate link should be refused")
	}

	prog, err := db.GetWorkflowProgress(ctx, w.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// s1 covered (not done yet); s2 a gap; s3 partly covered (s3.r1 a gap);
	// a1 a gap. A verifies link covers nothing.
	var gaps []string
	for _, g := range prog.Gaps {
		gaps = append(gaps, g.Key+":"+g.Kind)
	}
	if prog.Steps != 3 || prog.Covered != 1 || prog.Ready != 0 ||
		strings.Join(gaps, " ") != "s2:step s3.r1:result a1:alternate" {
		t.Errorf("progress: steps=%d covered=%d ready=%d gaps=%v", prog.Steps, prog.Covered, prog.Ready, gaps)
	}

	// Done tickets make a covered step ready; a deleted ticket stops covering.
	db.UpdateItemStatus(ctx, t1.ID, "done")
	if prog, _ = db.GetWorkflowProgress(ctx, w.ID, 0); prog.Ready != 1 {
		t.Errorf("ready after done: %d", prog.Ready)
	}
	db.SoftDelete(ctx, "item", t1.ID)
	if prog, _ = db.GetWorkflowProgress(ctx, w.ID, 0); prog.Covered != 0 {
		t.Errorf("deleted ticket still covers: %+v", prog)
	}

	// Reverse lookup, and unlink.
	onTicket, _ := db.ListWorkflowLinks(ctx, "", t3.ID)
	if len(onTicket) != 1 || onTicket[0].PartKey != "s3.r2" {
		t.Errorf("links by ticket: %+v", onTicket)
	}
	if err := db.UnlinkWorkflowTicket(ctx, w.ID, "s3.r2", t3.ID, "implements"); err != nil {
		t.Fatal(err)
	}
	if onTicket, _ = db.ListWorkflowLinks(ctx, "", t3.ID); len(onTicket) != 0 {
		t.Errorf("after unlink: %+v", onTicket)
	}
}

func TestProgressFollowsStepReferences(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	sub := mustWorkflow(t, db, p.ID, WorkflowDoc{Title: "Sign in", Goal: "g", Steps: []WorkflowStep{{Action: "enter credentials"}}})
	top := mustWorkflow(t, db, p.ID, WorkflowDoc{Title: "Top", Goal: "g", Steps: []WorkflowStep{{Ref: &WorkflowRef{WorkflowID: sub.ID}}}})

	prog, _ := db.GetWorkflowProgress(ctx, top.ID, 0)
	if prog.Covered != 1 || prog.Ready != 0 || len(prog.Gaps) != 0 {
		t.Errorf("reference step: %+v", prog)
	}
	done := mustTyped(t, db, p.ID, "task", "credentials form")
	db.LinkWorkflowTicket(ctx, sub.ID, "s1", done.ID, "implements")
	db.UpdateItemStatus(ctx, done.ID, "done")
	if prog, _ = db.GetWorkflowProgress(ctx, top.ID, 0); prog.Ready != 1 {
		t.Errorf("reference step should be ready once the referenced workflow is: %+v", prog)
	}
}

func TestDeletedWorkflowIsReadOnlyUntilRestored(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	w := mustWorkflow(t, db, p.ID, sampleDoc())
	if err := db.SoftDelete(ctx, "workflow", w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkflow(ctx, w.ID, w.Version.Doc, 1, ""); !errors.Is(err, ErrDeleted) {
		t.Errorf("update of deleted: %v", err)
	}
	if _, err := db.SetWorkflowStatus(ctx, w.ID, "active"); !errors.Is(err, ErrDeleted) {
		t.Errorf("status of deleted: %v", err)
	}
	if got, _ := db.ListWorkflows(ctx, WorkflowFilter{ProjectID: p.ID}); len(got) != 0 {
		t.Errorf("deleted workflow listed: %+v", got)
	}
	if g, err := db.GetWorkflow(ctx, w.ID, 0); err != nil || !g.DeletedAt.Valid {
		t.Errorf("deleted workflow should still be readable for inspection: %v", err)
	}
	if err := db.Restore(ctx, "workflow", w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetWorkflowStatus(ctx, w.ID, "active"); err != nil {
		t.Errorf("after restore: %v", err)
	}
}

func TestWorkflowVocabularyTriggers(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	w := mustWorkflow(t, db, p.ID, sampleDoc())
	// Writes that bypass this package are still held to the vocabulary.
	if _, err := db.conn.Exec(`UPDATE workflows SET status = 'live' WHERE id = ?`, w.ID); err == nil {
		t.Error("trigger should reject an off-vocabulary workflow status")
	}
	it := mustTyped(t, db, p.ID, "task", "t")
	if _, err := db.conn.Exec(`INSERT INTO workflow_links (id, workflow_id, part_key, item_id, role, created_at)
		VALUES ('x', ?, 's1', ?, 'owns', 'now')`, w.ID, it.ID); err == nil {
		t.Error("trigger should reject an off-vocabulary link role")
	}
}

func TestWorkflowScopeFeaturesConstraints(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	d := sampleDoc()
	d.InScope = []WorkflowItem{{Text: "creating public projects"}}
	d.OutOfScope = []WorkflowItem{{Text: "team projects"}, {Text: "importing projects"}}
	d.Features = []WorkflowItem{{Text: "visibility flag"}, {Text: "owner list"}}
	d.Constraints = []WorkflowItem{{Text: "works without JavaScript for reads"}}
	w := mustWorkflow(t, db, p.ID, d)

	got := w.Version.Doc
	keys := func(items []WorkflowItem) string {
		var k []string
		for _, it := range items {
			k = append(k, it.Key)
		}
		return strings.Join(k, " ")
	}
	if keys(got.InScope) != "i1" || keys(got.OutOfScope) != "o1 o2" || keys(got.Features) != "f1 f2" || keys(got.Constraints) != "c1" {
		t.Errorf("keys: in=%s out=%s features=%s constraints=%s", keys(got.InScope), keys(got.OutOfScope), keys(got.Features), keys(got.Constraints))
	}

	// Features are gaps until implemented; scope and constraints never are.
	prog, _ := db.GetWorkflowProgress(ctx, w.ID, 0)
	var featureGaps []string
	for _, g := range prog.Gaps {
		switch g.Kind {
		case "feature":
			featureGaps = append(featureGaps, g.Key)
		case "in_scope", "out_of_scope", "constraint":
			t.Errorf("%s should never be a gap: %+v", g.Kind, g)
		}
	}
	if strings.Join(featureGaps, " ") != "f1 f2" {
		t.Errorf("feature gaps: %v", featureGaps)
	}
	task := mustTyped(t, db, p.ID, "task", "build the flag")
	if _, err := db.LinkWorkflowTicket(ctx, w.ID, "f1", task.ID, "implements"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LinkWorkflowTicket(ctx, w.ID, "c1", task.ID, "verifies"); err != nil {
		t.Errorf("constraints are linkable: %v", err)
	}
	prog, _ = db.GetWorkflowProgress(ctx, w.ID, 0)
	for _, g := range prog.Gaps {
		if g.Key == "f1" {
			t.Error("an implemented feature is still a gap")
		}
	}

	// Removing o1 and adding another out-of-scope item does not reuse o1;
	// empty text is refused.
	got.OutOfScope = []WorkflowItem{got.OutOfScope[1], {Text: "archiving"}}
	w2, err := db.UpdateWorkflow(ctx, w.ID, got, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if k := keys(w2.Version.Doc.OutOfScope); k != "o2 o3" {
		t.Errorf("out-of-scope keys after edit: %s (want o2 o3)", k)
	}
	bad := w2.Version.Doc
	bad.Constraints = append(bad.Constraints, WorkflowItem{Text: " "})
	if _, err := db.UpdateWorkflow(ctx, w.ID, bad, 2, ""); err == nil || !strings.Contains(err.Error(), "constraint 2: text is required") {
		t.Errorf("empty constraint: %v", err)
	}
}

func TestSearchWorkflows(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	q := mustProject(t, db, "other")
	mustWorkflow(t, db, p.ID, sampleDoc())
	mustWorkflow(t, db, q.ID, WorkflowDoc{Title: "Close a project", Goal: "closed", Steps: []WorkflowStep{{Action: "archive"}}})

	for _, c := range []struct {
		project, query string
		want           int
	}{
		{"", "new-project form", 1}, // text inside a result
		{"", "PROJECT", 2},          // case-insensitive, titles
		{p.ID, "project", 1},        // narrowed to a project
		{"", "nowhere", 0},
	} {
		got, err := db.SearchWorkflows(ctx, c.project, c.query)
		if err != nil || len(got) != c.want {
			t.Errorf("search %q in %q: %d results, %v (want %d)", c.query, c.project, len(got), err, c.want)
		}
	}
}

func TestDiffWorkflowVersions(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	w := mustWorkflow(t, db, p.ID, sampleDoc())
	d := w.Version.Doc
	d.Goal = "changed goal"
	d.Steps = []WorkflowStep{d.Steps[1], d.Steps[0], d.Steps[2]} // s1 and s2 swap
	d.Steps[2].Action = "Click Save now"                         // s3 changes
	d.Steps[2].Results = d.Steps[2].Results[:1]                  // s3.r2 removed
	d.Features = []WorkflowItem{{Text: "a feature"}}             // f1 added
	d.Parameters[0].Value = "http://example.test"
	if _, err := db.UpdateWorkflow(ctx, w.ID, d, 1, ""); err != nil {
		t.Fatal(err)
	}
	changes, err := db.DiffWorkflowVersions(ctx, w.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range changes {
		got[c.Key] = c.Change
	}
	want := map[string]string{"goal": "changed", "f1": "added", "s1": "moved", "s2": "moved", "s3": "changed",
		"s3.r2": "removed", "parameter:base_url": "changed"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected extra changes: %v", got)
	}
	if none, _ := db.DiffWorkflowVersions(ctx, w.ID, 2, 2); len(none) != 0 {
		t.Errorf("a version against itself: %v", none)
	}
}

func TestCreateGapTickets(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	story := mustTyped(t, db, p.ID, "story", "Story")
	w := mustWorkflow(t, db, p.ID, sampleDoc())
	before, _ := db.GetWorkflowProgress(ctx, w.ID, 0)

	// A subset, including a key that is not a gap.
	res, err := db.CreateGapTickets(ctx, w.ID, []string{"s1", "p1"}, story.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].ItemID == "" || res[0].Error != nil || res[1].Error == nil {
		t.Fatalf("subset: %+v", res)
	}
	it, _ := db.GetItem(ctx, res[0].ItemID)
	if it.Title != "Open {base_url}" || it.ParentID.String != story.ID || it.Type != "task" {
		t.Errorf("created ticket: %+v", it)
	}

	// All remaining gaps.
	res, err = db.CreateGapTickets(ctx, w.ID, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(before.Gaps)-1 {
		t.Errorf("remaining gaps: created %d, want %d", len(res), len(before.Gaps)-1)
	}
	after, _ := db.GetWorkflowProgress(ctx, w.ID, 0)
	if len(after.Gaps) != 0 {
		t.Errorf("gaps left: %+v", after.Gaps)
	}
	// One batch id per call.
	audit, _ := db.ListAuditLog(ctx, AuditFilter{EntityType: "workflow_link"})
	batches := map[string]bool{}
	for _, a := range audit {
		batches[a.Batch.String] = true
	}
	if len(batches) != 2 {
		t.Errorf("want 2 batches (one per call), got %d", len(batches))
	}
}

func TestWorkflowHealthFindings(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "wf")
	mustTyped(t, db, p.ID, "task", "keeps the project non-empty")
	sub := mustWorkflow(t, db, p.ID, WorkflowDoc{Title: "Sub", Goal: "g", Steps: []WorkflowStep{{Action: "a"}}})
	long := WorkflowDoc{Title: "Long", Goal: "g", Steps: []WorkflowStep{{Ref: &WorkflowRef{WorkflowID: sub.ID}}}}
	for i := 0; i < 10; i++ {
		long.Steps = append(long.Steps, WorkflowStep{Action: "step"})
	}
	w := mustWorkflow(t, db, p.ID, long)
	gone := mustTyped(t, db, p.ID, "task", "to be deleted")
	orphan := mustTyped(t, db, p.ID, "task", "on a removed step")
	db.LinkWorkflowTicket(ctx, w.ID, "s2", gone.ID, "implements")
	db.LinkWorkflowTicket(ctx, w.ID, "s11", orphan.ID, "implements")
	db.SoftDelete(ctx, "item", gone.ID)
	d := w.Version.Doc
	d.Steps = d.Steps[:10] // drop s11
	if _, err := db.UpdateWorkflow(ctx, w.ID, d, 1, ""); err != nil {
		t.Fatal(err)
	}
	db.SetWorkflowStatus(ctx, sub.ID, "archived")

	findings, err := db.HealthFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, f := range findings {
		got[f.Check]++
	}
	want := map[string]int{"workflow_unassociated": 1, "workflow_link_deleted_ticket": 1, "workflow_link_orphaned": 1,
		"workflow_ref_retired": 1, "workflow_long": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %d findings, want %d (all: %v)", k, got[k], v, got)
		}
	}
}
