package ledgercore

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// The smoke tests run the copier against a COPY of a real ledger and a throwaway PostgreSQL.
// They are gated: scripts/smoke-copy.sh takes the copy, starts the server and sets
// LEDGERCORE_SMOKE_SOURCE (the copy) and LEDGERCORE_TEST_PG. Nothing real is ever written:
// the temporary project lives only in the copy, and the PostgreSQL schemas are dropped afterwards.

func smokeSource(t *testing.T) *DB {
	t.Helper()
	path := os.Getenv("LEDGERCORE_SMOKE_SOURCE")
	if path == "" || !onPostgres() {
		t.Skip("smoke test: run scripts/smoke-copy.sh (needs LEDGERCORE_SMOKE_SOURCE and LEDGERCORE_TEST_PG)")
	}
	return openSQLiteFile(t, path, Actor{Kind: ActorAgent, ID: "claude-code", Label: "claude-code"})
}

// buildSmokeProject creates one temporary project with every kind of data a ledger holds.
func buildSmokeProject(t *testing.T, db *DB, key string) (*Project, *Item) {
	t.Helper()
	p, err := db.GetOrCreateProjectForWrite(ctx, key, key)
	if err != nil {
		t.Fatal(err)
	}
	epic, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Type: "epic", Title: "smoke epic", Labels: []string{"smoke", "Needs Review"}, Description: "a description\nwith two lines and ünïcode"})
	story, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Type: "story", Title: "smoke story", ParentID: epic.ID, Assignees: []string{"person:bob", "agent:claude-code"}, Watchers: []string{"person:carol"}})
	task, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "smoke task", ParentID: story.ID, Status: "in_progress"})
	gone, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "smoke deleted task"})
	plan, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Type: "plan", Title: "smoke plan"})
	pr := 7
	db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "smoke with priority", Priority: &pr})

	db.AddNote(ctx, AddNoteParams{ItemID: task.ID, Body: "a note on the task"})
	db.AddNote(ctx, AddNoteParams{ProjectID: p.ID, Body: "a project-level note"})
	db.StartTimer(ctx, task.ID)
	db.StopTimer(ctx, task.ID)
	db.AddResource(ctx, p.ID, task.ID, "https://example.test/task", "spec")
	db.AddResource(ctx, p.ID, "", "https://example.test/home", "home")
	db.AddProjectMember(ctx, p.ID, "person:bob", "member")
	db.AddProjectWatcher(ctx, p.ID, "person:carol")
	db.UpdateItemStatus(ctx, task.ID, "blocked")
	db.UpdateItemStatus(ctx, task.ID, "in_progress")
	db.TransferItemOwner(ctx, story.ID, "person:dave")
	db.AddLabel(ctx, story.ID, "late")
	db.RemoveLabel(ctx, story.ID, "late") // a removed label: a soft-deleted row
	db.RelateItems(ctx, task.ID, story.ID, "related_to")
	db.RelateItems(ctx, task.ID, plan.ID, "part_of")
	wf := mustWorkflow(t, db, p.ID, sampleDoc())
	db.AssociateWorkflow(ctx, wf.ID, epic.ID, 0)
	if _, err := db.LinkWorkflowTicket(ctx, wf.ID, "s1", task.ID, "implements"); err != nil {
		t.Fatal(err)
	}
	db.SoftDelete(ctx, "item", gone.ID)
	return p, task
}

func TestSmoke_aTemporaryProjectIsCopiedToPostgresAndVerified(t *testing.T) {
	src := smokeSource(t)
	key := "smoke-tmp"
	p, task := buildSmokeProject(t, src, key)

	// Phase 1: the temporary project is linked to nothing else, so no decision is needed.
	dst := copyTarget(t)
	opts := CopyOptions{Scope: CopyScope{Projects: []string{key}}, Apply: true}
	rep, err := CopyLedger(ctx, src, dst, opts)
	if err != nil {
		t.Fatalf("phase 1 copy: %v", err)
	}
	// Six tickets; seven assignees, because the project was written by an agent and an agent's
	// tickets are assigned to it by default (five of them) plus the story's two named ones.
	for table, want := range map[string]int{"projects": 1, "items": 6, "item_labels": 3, "item_assignees": 7, "item_watchers": 1, "workflows": 1} {
		if got := rep.table(table); got.Inserted != want {
			t.Errorf("phase 1: %s inserted %d, want %d", table, got.Inserted, want)
		}
	}
	if rep.table("audit_log").Inserted < 30 || rep.table("notes").Inserted < 4 {
		t.Errorf("phase 1: the history did not come across: audit %d, notes %d", rep.table("audit_log").Inserted, rep.table("notes").Inserted)
	}
	v, err := VerifyCopy(ctx, src, dst, opts)
	if err != nil || !v.OK {
		t.Fatalf("phase 1 verification: %v %+v", err, v)
	}
	// And the PostgreSQL ledger works as a ledger: read it back through the Store.
	got, err := dst.GetItem(ctx, task.ID)
	if err != nil || got.Title != "smoke task" || got.Status != "in_progress" {
		t.Fatalf("reading the copied task: %+v %v", got, err)
	}
	if tl, err := dst.ListNotes(ctx, task.ID, ""); err != nil || len(tl) < 3 {
		t.Errorf("the timer events and note: %d %v", len(tl), err)
	}
	if _, err := dst.UpdateItemStatus(ctx, task.ID, "done"); err != nil {
		t.Errorf("the copied ledger cannot be written to: %v", err)
	}

	// Phase 2: link the temporary project to a REAL project and watch the decision flow.
	var realProject, realItem string
	if err := src.conn.QueryRow(`SELECT p.key, i.id FROM items i JOIN projects p ON p.id = i.project_id
		WHERE p.deleted_at IS NULL AND i.deleted_at IS NULL AND p.key <> ? ORDER BY p.key LIMIT 1`, key).Scan(&realProject, &realItem); err != nil {
		t.Skipf("phase 2 needs a real project in the ledger copy: %v", err)
	}
	if _, err := src.RelateItems(ctx, task.ID, realItem, "depends_on"); err != nil {
		t.Fatal(err)
	}
	_, err = CopyLedger(ctx, src, copyTarget(t), CopyOptions{Scope: CopyScope{Projects: []string{key}}, Apply: true})
	var cl *CrossLinksError
	if !errors.As(err, &cl) || !strings.Contains(err.Error(), realProject) {
		t.Fatalf("phase 2: linking to %q should need a decision naming it: %v", realProject, err)
	}
	dst2 := copyTarget(t)
	opts2 := CopyOptions{Scope: CopyScope{Projects: []string{key}}, DropCrossLinks: true, Apply: true}
	rep2, err := CopyLedger(ctx, src, dst2, opts2)
	if err != nil || len(rep2.Dropped) == 0 {
		t.Fatalf("phase 2 reject-all: %v %+v", err, rep2)
	}
	if v2, err := VerifyCopy(ctx, src, dst2, opts2); err != nil || !v2.OK {
		t.Fatalf("phase 2 verification after rejecting the links: %v %+v", err, v2)
	}
	// Including the real project copies a real project's data into PostgreSQL and keeps the link.
	dst3 := copyTarget(t)
	opts3 := CopyOptions{Scope: CopyScope{Projects: []string{key}}, WithLinked: []string{realProject}, Apply: true}
	rep3, err := CopyLedger(ctx, src, dst3, opts3)
	if err != nil {
		t.Fatalf("phase 2 include %q: %v", realProject, err)
	}
	if v3, err := VerifyCopy(ctx, src, dst3, opts3); err != nil || !v3.OK {
		t.Fatalf("phase 2 verification after including %q: %v %+v", realProject, err, v3)
	}
	if n := rows(t, dst3, `SELECT COUNT(*) FROM item_relations WHERE from_item_id = ? AND to_item_id = ?`, task.ID, realItem); n != 1 {
		t.Errorf("the link between the temporary project and %q on the target: %d", realProject, n)
	}
	t.Logf("phase 2: linked to %q; copied %d projects (%d items) to PostgreSQL, %d link(s) left out", realProject, len(rep3.Projects), rep3.table("items").Inserted, len(rep3.Dropped))
	_ = p
}

// Everything: every live project of the ledger copy into PostgreSQL. Optional, because it is the
// rehearsal of the real move: LEDGERCORE_SMOKE_ALL=1.
func TestSmoke_everyLiveProjectIntoPostgres(t *testing.T) {
	src := smokeSource(t)
	if os.Getenv("LEDGERCORE_SMOKE_ALL") != "1" {
		t.Skip("set LEDGERCORE_SMOKE_ALL=1 to rehearse copying every live project")
	}
	dst := copyTarget(t)
	// Rows from before the vocabulary rules existed (soft-deleted leftovers in the real ledger)
	// are refused by the target: that is a decision the operator makes, here by skipping them.
	var legacy int
	src.conn.QueryRow(`SELECT COUNT(*) FROM item_relations WHERE relation_type NOT IN ('blocked_by','depends_on','related_to','part_of')`).Scan(&legacy)
	_, err := CopyLedger(ctx, src, copyTarget(t), CopyOptions{Scope: CopyScope{All: true}, Apply: true})
	var ve *VocabularyError
	if legacy > 0 && !errors.As(err, &ve) {
		t.Fatalf("%d row(s) outside the vocabulary should need a decision: %v", legacy, err)
	}
	opts := CopyOptions{Scope: CopyScope{All: true}, SkipInvalid: true, Apply: true}
	rep, err := CopyLedger(ctx, src, dst, opts)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	// Only rows inside the copied (live) projects are skipped; the rest belong to soft-deleted projects.
	if len(rep.Skipped) > legacy {
		t.Errorf("skipped %d, but the source has only %d rows outside the vocabulary: %v", len(rep.Skipped), legacy, rep.Skipped)
	}
	v, err := VerifyCopy(ctx, src, dst, opts)
	if err != nil || !v.OK {
		t.Fatalf("verification: %v %+v", err, v)
	}
	for _, tb := range rep.Tables {
		if tb.Selected > 0 {
			t.Logf("%-24s selected %6d inserted %6d", tb.Table, tb.Selected, tb.Inserted)
		}
	}
	t.Logf("%d projects copied; %d link(s) left out (only those of soft-deleted projects expected)", len(rep.Projects), len(rep.Dropped))
	// A soft-deleted project may still point at a live one; that link is left out by design.
	// Any other link left out would mean a live project lost something.
	for _, l := range rep.Dropped {
		var deleted int
		src.conn.QueryRow(`SELECT COUNT(*) FROM projects WHERE key = ? AND deleted_at IS NOT NULL`, l.Project).Scan(&deleted)
		if deleted == 0 {
			t.Errorf("copying every live project left out a link to a live project: %+v", l)
		}
	}
}
