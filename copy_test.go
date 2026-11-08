package ledgercore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The copier moves selected projects from one ledger into another, preserving ids so
// that links keep meaning and a second run is a no-op. The source here is always a
// SQLite file; the target is a second SQLite file, or PostgreSQL when the suite runs
// with LEDGERCORE_TEST_PG (make test-pg), so that run exercises SQLite to PostgreSQL.

func openSQLiteFile(t *testing.T, path string, actor Actor) *DB {
	t.Helper()
	s, err := Open(Options{Path: path, Actor: func(context.Context) Actor { return actor }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s.(*DB)
}

// copyTarget is an empty ledger of whichever kind this run targets.
func copyTarget(t *testing.T) *DB {
	t.Helper()
	return openAs(t, filepath.Join(t.TempDir(), "dst.db"), script, nil)
}

type copyFx struct {
	src                *DB
	alpha, beta, gamma *Project
	deleted            *Project
	a1, a2, a3, a4, a5 *Item // alpha; a4's parent is in beta, a5 is soft-deleted
	b1, b2             *Item
	c1                 *Item
	plan               *Item // a plan in beta whose members are in alpha and beta
	wfA                *Workflow
}

func newCopyFixture(t *testing.T) *copyFx {
	t.Helper()
	db := openSQLiteFile(t, filepath.Join(t.TempDir(), "src.db"), alice)
	f := &copyFx{src: db}
	f.alpha, f.beta, f.gamma = mustProject(t, db, "alpha"), mustProject(t, db, "beta"), mustProject(t, db, "gamma")
	f.deleted = mustProject(t, db, "delta")
	mustItem(t, db, f.deleted.ID, "in the deleted project")

	mk := func(p *Project, title string, labels ...string) *Item {
		it, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: title, Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	f.a1 = mk(f.alpha, "a1 epic", "core")
	f.a2, _ = db.CreateItem(ctx, CreateItemParams{ProjectID: f.alpha.ID, Title: "a2", ParentID: f.a1.ID, Labels: []string{"x", "y"}, Assignees: []string{"person:bob"}, Watchers: []string{"person:carol"}})
	f.a3 = mk(f.alpha, "a3")
	f.b1, f.b2 = mk(f.beta, "b1"), mk(f.beta, "b2", "beta-only")
	f.a4, _ = db.CreateItem(ctx, CreateItemParams{ProjectID: f.alpha.ID, Title: "a4 child of b1", ParentID: f.b1.ID})
	f.a5 = mk(f.alpha, "a5 deleted")
	f.c1, _ = db.CreateItem(ctx, CreateItemParams{ProjectID: f.gamma.ID, Title: "c1", Labels: []string{"solo"}, Assignees: []string{"person:bob"}})
	f.plan, _ = db.CreateItem(ctx, CreateItemParams{ProjectID: f.beta.ID, Type: "plan", Title: "the plan"})

	db.AddNote(ctx, AddNoteParams{ItemID: f.a2.ID, Body: "a note"})
	db.AddNote(ctx, AddNoteParams{ProjectID: f.alpha.ID, Body: "a project-level note"})
	db.StartTimer(ctx, f.a3.ID)
	db.StopTimer(ctx, f.a3.ID)
	db.AddResource(ctx, f.alpha.ID, f.a1.ID, "https://example.test/a1", "spec")
	db.AddResource(ctx, f.alpha.ID, "", "https://example.test/project", "home")
	db.AddProjectMember(ctx, f.alpha.ID, "person:bob", "member")
	db.AddProjectWatcher(ctx, f.alpha.ID, "person:carol")
	db.UpdateItemStatus(ctx, f.a2.ID, "in_progress")
	db.TransferItemOwner(ctx, f.a2.ID, "person:dave")

	db.RelateItems(ctx, f.a2.ID, f.a3.ID, "related_to") // inside alpha
	db.RelateItems(ctx, f.a1.ID, f.b1.ID, "depends_on") // alpha -> beta
	db.RelateItems(ctx, f.a3.ID, f.plan.ID, "part_of")  // a plan member in alpha
	db.RelateItems(ctx, f.b2.ID, f.plan.ID, "part_of")  // a plan member in beta

	f.wfA = mustWorkflow(t, db, f.alpha.ID, sampleDoc())
	db.AssociateWorkflow(ctx, f.wfA.ID, f.a1.ID, 0)
	if _, err := db.LinkWorkflowTicket(ctx, f.wfA.ID, "s1", f.b2.ID, "implements"); err != nil { // links into beta
		t.Fatal(err)
	}
	if _, err := db.LinkWorkflowTicket(ctx, f.wfA.ID, "s1", f.a3.ID, "implements"); err != nil {
		t.Fatal(err)
	}
	db.SoftDelete(ctx, "item", f.a5.ID)
	db.SoftDelete(ctx, "project", f.deleted.ID)
	return f
}

func mustCopy(t *testing.T, f *copyFx, dst *DB, o CopyOptions) *CopyReport {
	t.Helper()
	rep, err := CopyLedger(ctx, f.src, dst, o)
	if err != nil {
		t.Fatalf("CopyLedger: %v", err)
	}
	return rep
}

func rows(t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.conn.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (r *CopyReport) table(name string) CopyTable {
	for _, tb := range r.Tables {
		if tb.Table == name {
			return tb
		}
	}
	return CopyTable{}
}

func keys(r *CopyReport) []string {
	k := append([]string(nil), r.Projects...)
	sort.Strings(k)
	return k
}

// ---- an isolated project: no decision needed ----

func TestCopy_anIsolatedProjectCopiesCompletelyAndIsVerified(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)

	rep := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true})
	if !rep.Applied || !reflect.DeepEqual(keys(rep), []string{"gamma"}) {
		t.Fatalf("report: %+v", rep)
	}
	if rep.table("items").Inserted != 1 || rep.table("projects").Inserted != 1 || rep.table("item_labels").Inserted != 1 {
		t.Errorf("tables: %+v", rep.Tables)
	}
	// What arrived is the same ticket, with the same id and everything about it.
	got, err := dst.GetItem(ctx, f.c1.ID)
	if err != nil || got.Title != "c1" || got.CreatedBy.String != f.c1.CreatedBy.String || got.CreatedAt != f.c1.CreatedAt || got.UpdatedAt != f.c1.UpdatedAt {
		t.Fatalf("copied item: %+v %v", got, err)
	}
	if !reflect.DeepEqual(got.Labels, []string{"solo"}) || !reflect.DeepEqual(got.Assignees, []string{"person:bob"}) {
		t.Errorf("labels/assignees: %v %v", got.Labels, got.Assignees)
	}
	// The audit trail came with it, in the same order.
	srcAudit, _ := f.src.ListAuditLog(ctx, AuditFilter{EntityID: f.c1.ID})
	dstAudit, _ := dst.ListAuditLog(ctx, AuditFilter{EntityID: f.c1.ID})
	if len(srcAudit) == 0 || len(srcAudit) != len(dstAudit) {
		t.Fatalf("audit rows: %d vs %d", len(srcAudit), len(dstAudit))
	}
	for i := range srcAudit {
		if srcAudit[i].ID != dstAudit[i].ID || srcAudit[i].Actor != dstAudit[i].Actor {
			t.Errorf("audit row %d differs: %+v vs %+v", i, srcAudit[i], dstAudit[i])
		}
	}
	// Nothing from the other projects leaked in.
	if n := rows(t, dst, `SELECT COUNT(*) FROM projects`); n != 1 {
		t.Errorf("projects in the target: %d", n)
	}
}

func TestCopy_aDryRunWritesNothingButReportsTheSame(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	dry := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}})
	if dry.Applied {
		t.Error("a dry run reports applied")
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM projects`) + rows(t, dst, `SELECT COUNT(*) FROM items`) + rows(t, dst, `SELECT COUNT(*) FROM audit_log`); n != 0 {
		t.Fatalf("a dry run wrote %d rows", n)
	}
	real := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true})
	if !reflect.DeepEqual(dry.Tables, real.Tables) {
		t.Errorf("dry run and real run disagree:\n%+v\n%+v", dry.Tables, real.Tables)
	}
}

func TestCopy_runningTwiceChangesNothing(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true})
	again := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true})
	for _, tb := range again.Tables {
		if tb.Inserted != 0 || tb.AlreadyThere != tb.Selected {
			t.Errorf("second run, %s: %+v", tb.Table, tb)
		}
	}
}

// ---- links to projects that were not chosen ----

func TestCopy_linksToOtherProjectsNeedADecision(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	_, err := CopyLedger(ctx, f.src, dst, CopyOptions{Scope: CopyScope{Projects: []string{"alpha"}}, Apply: true})
	var cl *CrossLinksError
	if !errors.As(err, &cl) {
		t.Fatalf("expected a *CrossLinksError, got %v", err)
	}
	var beta []CrossLink
	for _, l := range cl.Links {
		if l.Project == "beta" {
			beta = append(beta, l)
		}
		if l.Project == "gamma" || l.Project == "delta" {
			t.Errorf("an unlinked project is listed: %+v", l)
		}
	}
	kinds := ""
	for _, l := range beta {
		kinds += l.Kind + ";"
	}
	for _, want := range []string{"relation", "parent", "workflow link"} {
		if !strings.Contains(kinds, want) {
			t.Errorf("the notification lacks a %q link to beta: %v", want, beta)
		}
	}
	if !strings.Contains(err.Error(), "beta") || !strings.Contains(err.Error(), "-with-linked") || !strings.Contains(err.Error(), "-drop-cross-links") {
		t.Errorf("the message must name the project and the choices: %v", err)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM projects`); n != 0 {
		t.Errorf("a cancelled copy wrote %d projects", n)
	}
}

func TestCopy_rejectingTheLinksLeavesThemOutAndSaysSo(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	rep := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"alpha"}}, DropCrossLinks: true, Apply: true})
	if !reflect.DeepEqual(keys(rep), []string{"alpha"}) {
		t.Fatalf("projects: %v", rep.Projects)
	}
	// The ticket arrived, its link to beta did not, and the report names what was left out.
	if _, err := dst.GetItem(ctx, f.a1.ID); err != nil {
		t.Fatal(err)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM item_relations WHERE from_item_id = ?`, f.a1.ID); n != 0 {
		t.Errorf("the link to beta was copied: %d", n)
	}
	a4, _ := dst.GetItem(ctx, f.a4.ID)
	if a4.ParentID.Valid {
		t.Errorf("a ticket whose parent is in beta kept a dangling parent: %v", a4.ParentID)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM workflow_links`); n != 1 {
		t.Errorf("workflow links copied: %d, want only the one inside alpha", n)
	}
	got := map[string]int{}
	for _, l := range rep.Dropped {
		got[l.Kind] += l.Count
	}
	if got["parent"] != 1 || got["workflow link"] != 1 {
		t.Errorf("dropped links: %+v", rep.Dropped)
	}
	relDropped := 0
	for k, n := range got {
		if strings.HasPrefix(k, "relation") {
			relDropped += n
		}
	}
	if relDropped != 2 { // alpha->beta depends_on, and the plan membership alpha->plan in beta
		t.Errorf("relations left out: %d (%+v)", relDropped, rep.Dropped)
	}
	// Choosing beta later completes the links, because ids were preserved: the relations are
	// added, and the one parent link the first copy had to cut is filled in (an update, counted).
	rep2 := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"alpha"}}, WithLinked: []string{"beta"}, Apply: true})
	if len(rep2.Dropped) != 0 {
		t.Errorf("nothing should be left out the second time: %+v", rep2.Dropped)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM item_relations WHERE from_item_id = ? AND to_item_id = ?`, f.a1.ID, f.b1.ID); n != 1 {
		t.Errorf("the link to beta after completing the copy: %d", n)
	}
	if rep2.table("items").Updated != 1 {
		t.Errorf("items updated: %+v", rep2.table("items"))
	}
	fixed, _ := dst.GetItem(ctx, f.a4.ID)
	if fixed.ParentID.String != f.b1.ID {
		t.Errorf("a4's parent was not filled in: %v", fixed.ParentID)
	}
}

func TestCopy_includingNamedLinkedProjectsKeepsTheirLinks(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	rep := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"alpha"}}, WithLinked: []string{"beta"}, Apply: true})
	if !reflect.DeepEqual(keys(rep), []string{"alpha", "beta"}) {
		t.Fatalf("projects: %v", rep.Projects)
	}
	if len(rep.Dropped) != 0 {
		t.Errorf("nothing should be left out: %+v", rep.Dropped)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM item_relations WHERE from_item_id = ? AND to_item_id = ?`, f.a1.ID, f.b1.ID); n != 1 {
		t.Errorf("the link between alpha and beta: %d", n)
	}
	a4, _ := dst.GetItem(ctx, f.a4.ID)
	if a4.ParentID.String != f.b1.ID {
		t.Errorf("a4's parent in beta: %v", a4.ParentID)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM workflow_links`); n != 2 {
		t.Errorf("workflow links: %d", n)
	}
	// gamma was not asked for and not linked: it stays behind.
	if n := rows(t, dst, `SELECT COUNT(*) FROM projects WHERE key = 'gamma'`); n != 0 {
		t.Error("an unrelated project was copied")
	}
	// A name that is not a project is an error, not a silent no-op.
	if _, err := CopyLedger(ctx, f.src, copyTarget(t), CopyOptions{Scope: CopyScope{Projects: []string{"alpha"}}, WithLinked: []string{"nope"}, Apply: true}); err == nil {
		t.Error("an unknown linked project was accepted")
	}
}

// ---- plans and workflows are copied as the whole projects they belong to and reference ----

func TestCopy_aPlanBringsTheProjectsOfItsMembers(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	rep := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Plans: []string{f.plan.ID}}, Apply: true})
	if !reflect.DeepEqual(keys(rep), []string{"alpha", "beta"}) {
		t.Fatalf("a plan in beta with members in alpha and beta should copy both: %v", rep.Projects)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM items WHERE project_id = ?`, f.alpha.ID); n == 0 {
		t.Error("alpha was copied without its tickets: a project is copied whole")
	}
	if _, err := CopyLedger(ctx, f.src, copyTarget(t), CopyOptions{Scope: CopyScope{Plans: []string{f.a1.ID}}, Apply: true}); err == nil {
		t.Error("a ticket that is not a plan was accepted as one")
	}
}

func TestCopy_aWorkflowBringsTheProjectsItReferences(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	rep := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Workflows: []string{f.wfA.ID}}, Apply: true})
	if !reflect.DeepEqual(keys(rep), []string{"alpha", "beta"}) {
		t.Fatalf("a workflow in alpha linked to a ticket in beta should copy both: %v", rep.Projects)
	}
	if rep.table("workflows").Inserted != 1 || rep.table("workflow_versions").Inserted == 0 {
		t.Errorf("workflow tables: %+v", rep.Tables)
	}
}

// ---- everything, and what "everything" leaves out ----

func TestCopy_everythingMeansTheLiveProjects(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	rep := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{All: true}, Apply: true})
	if !reflect.DeepEqual(keys(rep), []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("projects: %v", rep.Projects)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM projects WHERE key = 'delta'`); n != 0 {
		t.Error("a deleted project was copied without asking")
	}
	// A deleted ticket inside a live project is copied as deleted, so a restore still works there.
	a5, err := dst.GetItem(ctx, f.a5.ID)
	if err != nil || !a5.DeletedAt.Valid {
		t.Fatalf("the deleted ticket: %+v %v", a5, err)
	}
	if err := dst.Restore(ctx, "item", f.a5.ID); err != nil {
		t.Errorf("restoring on the target: %v", err)
	}
	// Asking for deleted projects too:
	dst2 := copyTarget(t)
	rep2 := mustCopy(t, f, dst2, CopyOptions{Scope: CopyScope{All: true, IncludeDeleted: true}, Apply: true})
	if !reflect.DeepEqual(keys(rep2), []string{"alpha", "beta", "delta", "gamma"}) {
		t.Errorf("with deleted: %v", rep2.Projects)
	}
	p, err := dst2.GetProjectByID(ctx, f.deleted.ID)
	if err != nil || p.Key != "delta" {
		t.Errorf("the deleted project on the target: %+v %v", p, err)
	}
}

// ---- nothing is overwritten, and nothing is half-written ----

func TestCopy_aDifferingRowOnTheTargetStopsTheWholeCopy(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true})
	// Someone edits the ticket on the target.
	if _, err := dst.UpdateItem(ctx, f.c1.ID, UpdateItemParams{Title: ptr("edited on the target")}); err != nil {
		t.Fatal(err)
	}
	// A new ticket appears in the source too; it must not be copied by a refused run.
	extra, _ := f.src.CreateItem(ctx, CreateItemParams{ProjectID: f.gamma.ID, Title: "c2 added later"})

	_, err := CopyLedger(ctx, f.src, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true})
	var ce *CopyConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected a *CopyConflictError, got %v", err)
	}
	if !strings.Contains(strings.Join(ce.Conflicts, "\n"), f.c1.ID) {
		t.Errorf("the conflict should name the ticket: %v", ce.Conflicts)
	}
	got, _ := dst.GetItem(ctx, f.c1.ID)
	if got.Title != "edited on the target" {
		t.Errorf("the target's edit was overwritten: %q", got.Title)
	}
	if _, err := dst.GetItem(ctx, extra.ID); err == nil {
		t.Error("a refused copy still wrote the new ticket: it must be all or nothing")
	}
}

// dropVocabularyTriggers lets a SQLite source hold values the rules would refuse, as an older
// ledger could before the rules existed.
func dropVocabularyTriggers(t *testing.T, db *DB) {
	t.Helper()
	rs, err := db.conn.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'ledger\_vocab\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rs.Next() {
		var n string
		rs.Scan(&n)
		names = append(names, n)
	}
	rs.Close()
	for _, n := range names {
		if _, err := db.conn.Exec(`DROP TRIGGER ` + n); err != nil {
			t.Fatal(err)
		}
	}
}

// Rows from before the rules existed are listed together, and nothing is copied until the
// operator decides; the target's rules are never bypassed.
func TestCopy_rowsOutsideTheVocabularyNeedADecisionAndAreAllListed(t *testing.T) {
	f := newCopyFixture(t)
	dropVocabularyTriggers(t, f.src)
	for i, rt := range []string{"blocks", "follows_up_on"} {
		if _, err := f.src.conn.Exec(`INSERT INTO item_relations (id, from_item_id, to_item_id, relation_type, created_at) VALUES (?, ?, ?, ?, ?)`,
			fmt.Sprintf("legacy-%d", i), f.c1.ID, f.c1.ID, rt, nowUTC()); err != nil {
			t.Fatal(err)
		}
	}
	dst := copyTarget(t)
	_, err := CopyLedger(ctx, f.src, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true})
	var ve *VocabularyError
	if !errors.As(err, &ve) {
		t.Fatalf("want a *VocabularyError, got %v", err)
	}
	if len(ve.Rows) != 2 || !ve.Skippable {
		t.Errorf("both rows should be listed, and skippable (they are leaf rows): %+v", ve)
	}
	msg := err.Error()
	for _, want := range []string{"relation_type", "blocks", "follows_up_on", "legacy-0", "legacy-1", "-skip-invalid"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q: %v", want, msg)
		}
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM projects`); n != 0 {
		t.Errorf("a refused copy left %d projects behind", n)
	}

	// Leaving them out is a decision, and it is reported; the rest copies and verifies.
	opts := CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, SkipInvalid: true, Apply: true}
	rep := mustCopy(t, f, dst, opts)
	if len(rep.Skipped) != 2 || !strings.Contains(strings.Join(rep.Skipped, "\n"), "legacy-0") {
		t.Errorf("skipped rows must be named in the report: %v", rep.Skipped)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM item_relations WHERE id LIKE 'legacy-%'`); n != 0 {
		t.Errorf("a skipped row reached the target: %d", n)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM audit_log WHERE entity_id LIKE 'legacy-%'`); n != 0 {
		t.Errorf("audit rows of a skipped row reached the target: %d", n)
	}
	v := mustVerify(t, f, dst, opts)
	if !v.OK || len(v.Skipped) != 2 {
		t.Errorf("verification must agree with the copy about what was left out: %+v", v)
	}
	// Verifying without the decision asks for it, like the copy.
	if _, err := VerifyCopy(ctx, f.src, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}}); err == nil {
		t.Error("verify without the decision should refuse like the copy")
	}
}

// A ticket outside the vocabulary cannot be skipped: its labels, notes and children would be orphaned.
func TestCopy_aTicketOutsideTheVocabularyIsNeverSkipped(t *testing.T) {
	f := newCopyFixture(t)
	dropVocabularyTriggers(t, f.src)
	if _, err := f.src.conn.Exec(`UPDATE items SET status = 'weird' WHERE id = ?`, f.c1.ID); err != nil {
		t.Fatal(err)
	}
	_, err := CopyLedger(ctx, f.src, copyTarget(t), CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, SkipInvalid: true, Apply: true})
	var ve *VocabularyError
	if !errors.As(err, &ve) || ve.Skippable {
		t.Fatalf("a ticket with a bad status must be refused even with -skip-invalid, and not offered as skippable: %v", err)
	}
	if !strings.Contains(err.Error(), "status") || !strings.Contains(err.Error(), "weird") {
		t.Errorf("the message should name the value: %v", err)
	}
}

func TestCopy_dataInAColumnTheTargetLacksIsNeverLost(t *testing.T) {
	f := newCopyFixture(t)
	if err := ensureColumn(f.src.conn, "items", "label", "TEXT"); err != nil {
		t.Fatal(err)
	}
	f.src.conn.Exec(`UPDATE items SET label = 'legacy' WHERE id = ?`, f.c1.ID)
	_, err := CopyLedger(ctx, f.src, copyTarget(t), CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true})
	if err == nil || !strings.Contains(err.Error(), "items.label") {
		t.Fatalf("copying past data the target has no column for must be refused, naming it: %v", err)
	}
}

// ---- identities ----

func TestCopy_identitiesCanBeMappedForTheTarget(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	m := map[string]string{"person:alice": "person:alice-remote", "person:bob": "person:bob-remote"}
	rep := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, MapIdentity: m, Apply: true})
	if rep.IdentitiesMapped == 0 {
		t.Error("no identity was mapped")
	}
	got, _ := dst.GetItem(ctx, f.c1.ID)
	if got.CreatedBy.String != "person:alice-remote" || got.Owner.String != "person:alice-remote" || !reflect.DeepEqual(got.Assignees, []string{"person:bob-remote"}) {
		t.Errorf("created_by=%q owner=%q assignees=%v", got.CreatedBy.String, got.Owner.String, got.Assignees)
	}
	ms, _ := dst.ListProjectMembers(ctx, f.gamma.ID)
	if len(ms) != 1 || ms[0].Member != "person:alice-remote" {
		t.Errorf("members: %+v", ms)
	}
	a, _ := dst.ListAuditLog(ctx, AuditFilter{EntityID: f.c1.ID})
	for _, e := range a {
		if e.Actor.String == "person:alice" {
			t.Errorf("an audit row kept the local identity: %+v", e)
		}
	}
	// A mapping that is not a well-formed identity is refused before anything is read.
	if _, err := CopyLedger(ctx, f.src, copyTarget(t), CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, MapIdentity: map[string]string{"person:alice": "alice"}, Apply: true}); err == nil {
		t.Error("a malformed target identity was accepted")
	}
	// Running it again with the same mapping is still a no-op (mapped values compare equal).
	again := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, MapIdentity: m, Apply: true})
	for _, tb := range again.Tables {
		if tb.Inserted != 0 {
			t.Errorf("second mapped run inserted into %s", tb.Table)
		}
	}
}

// ---- guard rails ----

func TestCopy_refusesNonsense(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	for name, o := range map[string]CopyOptions{
		"nothing selected":       {Apply: true},
		"an unknown project":     {Scope: CopyScope{Projects: []string{"no-such-project"}}, Apply: true},
		"a deleted project":      {Scope: CopyScope{Projects: []string{"delta"}}, Apply: true},
		"an unknown workflow":    {Scope: CopyScope{Workflows: []string{"nope"}}, Apply: true},
		"both decisions at once": {Scope: CopyScope{Projects: []string{"alpha"}}, WithLinked: []string{"beta"}, DropCrossLinks: true, Apply: true},
	} {
		if _, err := CopyLedger(ctx, f.src, dst, o); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := CopyLedger(ctx, f.src, f.src, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true}); err == nil {
		t.Error("copying a ledger onto itself was accepted")
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM projects`); n != 0 {
		t.Errorf("refused runs wrote %d projects", n)
	}
}

// What the report counts is what is really there, table by table.
func TestCopy_reportMatchesTheTarget(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	rep := mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{All: true}, Apply: true})
	for _, tb := range rep.Tables {
		if tb.Selected == 0 {
			continue
		}
		var n int
		if err := dst.conn.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tb.Table)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != tb.Selected {
			t.Errorf("%s: the report says %d, the target has %d", tb.Table, tb.Selected, n)
		}
	}
}

// The read-back check exists for a target that does not store what it was given: a trigger,
// a collation, a type that coerces. Nothing in normal operation trips it, so the test makes a
// target that quietly upper-cases every ticket title, and the copy must refuse to commit.
func TestCopy_verificationCatchesAValueTheTargetChanged(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	stmts := []string{
		`CREATE TRIGGER tamper AFTER INSERT ON items BEGIN UPDATE items SET title = upper(NEW.title) WHERE id = NEW.id; END`,
	}
	if onPostgres() {
		stmts = []string{
			`CREATE FUNCTION tamper() RETURNS trigger LANGUAGE plpgsql AS $b$ BEGIN UPDATE items SET title = upper(NEW.title) WHERE id = NEW.id; RETURN NULL; END $b$`,
			`CREATE TRIGGER tamper AFTER INSERT ON items FOR EACH ROW EXECUTE FUNCTION tamper()`,
		}
	}
	for _, q := range stmts {
		if _, err := dst.conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_, err := CopyLedger(ctx, f.src, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true})
	if err == nil || !strings.Contains(err.Error(), "verification failed") || !strings.Contains(err.Error(), "title") {
		t.Fatalf("the copy must refuse to commit when what was stored differs from what was sent: %v", err)
	}
	if n := rows(t, dst, `SELECT COUNT(*) FROM projects`); n != 0 {
		t.Errorf("a failed verification left %d projects behind", n)
	}
}

// ---- VerifyCopy: confirming a copy afterwards, without writing anything ----

func mustVerify(t *testing.T, f *copyFx, dst *DB, o CopyOptions) *VerifyReport {
	t.Helper()
	rep, err := VerifyCopy(ctx, f.src, dst, o)
	if err != nil {
		t.Fatalf("VerifyCopy: %v", err)
	}
	return rep
}

func (r *VerifyReport) table(name string) VerifyTable {
	for _, tb := range r.Tables {
		if tb.Table == name {
			return tb
		}
	}
	return VerifyTable{}
}

func TestVerify_aCompleteCopyIsConfirmedRowForRow(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	opts := CopyOptions{Scope: CopyScope{Projects: []string{"alpha"}}, WithLinked: []string{"beta"}, Apply: true}
	mustCopy(t, f, dst, opts)

	rep := mustVerify(t, f, dst, opts)
	if !rep.OK {
		t.Fatalf("a copy just made must verify: %+v", rep)
	}
	total := 0
	for _, tb := range rep.Tables {
		if tb.Missing != 0 || tb.Different != 0 || tb.OnlyOnTarget != 0 || tb.Matching != tb.Selected {
			t.Errorf("%s: %+v", tb.Table, tb)
		}
		total += tb.Matching
	}
	if total == 0 {
		t.Error("verified nothing")
	}
	// The verification wrote nothing: the target is exactly as the copy left it.
	again := mustCopy(t, f, dst, opts)
	for _, tb := range again.Tables {
		if tb.Inserted != 0 || tb.Updated != 0 {
			t.Errorf("verifying changed the target (%s): %+v", tb.Table, tb)
		}
	}
}

func TestVerify_beforeAnyCopyEverythingIsMissing(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	rep := mustVerify(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}})
	if rep.OK {
		t.Fatal("an empty target verified as a copy")
	}
	for _, tb := range rep.Tables {
		if tb.Missing != tb.Selected || tb.Matching != 0 {
			t.Errorf("%s: %+v", tb.Table, tb)
		}
	}
}

func TestVerify_findsWhatWentMissingWhatDiffersAndWhatIsExtra(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	opts := CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, Apply: true}
	mustCopy(t, f, dst, opts)

	// Three kinds of divergence on the target.
	if _, err := dst.UpdateItem(ctx, f.c1.ID, UpdateItemParams{Title: ptr("changed on the target")}); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.conn.Exec(`DELETE FROM item_assignees WHERE item_id = ?`, f.c1.ID); err != nil {
		t.Fatal(err)
	}
	extra, err := dst.CreateItem(ctx, CreateItemParams{ProjectID: f.gamma.ID, Title: "added on the target"})
	if err != nil {
		t.Fatal(err)
	}

	rep := mustVerify(t, f, dst, opts)
	if rep.OK {
		t.Fatal("a diverged target verified as a copy")
	}
	if tb := rep.table("items"); tb.Different != 1 || tb.OnlyOnTarget != 1 || !strings.Contains(strings.Join(tb.Examples, "\n"), f.c1.ID) {
		t.Errorf("items: %+v", tb)
	}
	if tb := rep.table("item_assignees"); tb.Missing != 1 {
		t.Errorf("item_assignees: %+v", tb)
	}
	if tb := rep.table("audit_log"); tb.OnlyOnTarget == 0 {
		t.Errorf("the audit rows the target wrote afterwards should show as extra: %+v", tb)
	}
	if !strings.Contains(strings.Join(rep.table("items").Examples, "\n"), "title") {
		t.Errorf("an example should say which column differs: %v", rep.table("items").Examples)
	}
	_ = extra
	// Extra rows alone are information, not a failure: the target may have moved on.
	dst2 := copyTarget(t)
	mustCopy(t, f, dst2, opts)
	dst2.CreateItem(ctx, CreateItemParams{ProjectID: f.gamma.ID, Title: "newer work on the target"})
	if r := mustVerify(t, f, dst2, opts); !r.OK || r.table("items").OnlyOnTarget != 1 {
		t.Errorf("extra rows must be reported but not fail the verification: %+v", r)
	}
}

func TestVerify_followsTheSameDecisionsAsTheCopy(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	if _, err := VerifyCopy(ctx, f.src, dst, CopyOptions{Scope: CopyScope{Projects: []string{"alpha"}}}); err == nil {
		t.Fatal("verifying a linked selection without a decision should ask for one, like the copy")
	} else {
		var cl *CrossLinksError
		if !errors.As(err, &cl) {
			t.Fatalf("want a *CrossLinksError, got %v", err)
		}
	}
	opts := CopyOptions{Scope: CopyScope{Projects: []string{"alpha"}}, DropCrossLinks: true, Apply: true}
	mustCopy(t, f, dst, opts)
	if rep := mustVerify(t, f, dst, opts); !rep.OK || len(rep.Dropped) == 0 {
		t.Errorf("a copy that left links out verifies, and still reports what was left out: %+v", rep)
	}
}

func TestVerify_identityMappingMustMatchTheCopys(t *testing.T) {
	f := newCopyFixture(t)
	dst := copyTarget(t)
	m := map[string]string{"person:alice": "person:alice-remote", "person:bob": "person:bob-remote"}
	mustCopy(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, MapIdentity: m, Apply: true})
	if rep := mustVerify(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}, MapIdentity: m}); !rep.OK {
		t.Errorf("verifying with the same mapping: %+v", rep)
	}
	if rep := mustVerify(t, f, dst, CopyOptions{Scope: CopyScope{Projects: []string{"gamma"}}}); rep.OK {
		t.Error("verifying without the mapping should find the rewritten identities different")
	}
}
