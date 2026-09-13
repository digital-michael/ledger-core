package ledgercore

// Integration tests: every test opens a real SQLite ledger in a temp
// directory through the public Open(), exactly as mcp-local and ledger-server
// do. Tests that need to bypass this package (to prove the storage-level
// triggers hold on their own) use db.conn directly, which is only possible
// because these tests live inside the package.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const testClient = "ledgercore-test"

var ctx = context.Background()

// TestMain doubles as the entry point for the child writer processes used by
// TestConcurrentWritersAcrossProcesses: the test binary re-executes itself
// with LEDGERCORE_WRITER set, and in that mode runs runWriter instead of the
// test suite.
func TestMain(m *testing.M) {
	if os.Getenv("LEDGERCORE_WRITER") != "" {
		os.Exit(runWriter())
	}
	os.Exit(m.Run())
}

func ptr(s string) *string { return &s }

func openAt(t *testing.T, path string) *DB {
	t.Helper()
	s, err := Open(Options{Path: path, Actor: func(context.Context) Actor { return Actor{Kind: ActorProgram, ID: testClient, Label: testClient} }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s.(*DB)
}

func openTest(t *testing.T) *DB {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "ledger.db"))
}

func mustProject(t *testing.T, db *DB, key string) *Project {
	t.Helper()
	p, err := db.GetOrCreateProjectForWrite(ctx, key, key)
	if err != nil {
		t.Fatalf("GetOrCreateProjectForWrite: %v", err)
	}
	return p
}

func mustItem(t *testing.T, db *DB, projectID, title string) *Item {
	t.Helper()
	it, err := db.CreateItem(ctx, CreateItemParams{ProjectID: projectID, Title: title})
	if err != nil {
		t.Fatalf("CreateItem %q: %v", title, err)
	}
	return it
}

// auditRows returns every audit row for one entity, oldest first.
func auditRows(t *testing.T, db *DB, entityType, id string) []AuditEntry {
	t.Helper()
	rows, err := db.conn.Query(`SELECT id, entity_type, entity_id, operation, detail, created_at, client, actor, batch
		FROM audit_log WHERE entity_type = ? AND entity_id = ? ORDER BY rowid`, entityType, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.EntityType, &e.EntityID, &e.Operation, &e.Detail, &e.CreatedAt, &e.Client, &e.Actor, &e.Batch); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func wantErrContaining(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected an error containing %q, got: %v", substr, err)
	}
}

// ---------------------------------------------------------------------------
// Opening the database
// ---------------------------------------------------------------------------

// The ledger's location must never move: every program opening it has to
// resolve the same file, or a new build silently starts on an empty database.
func TestDefaultPathResolution(t *testing.T) {
	t.Setenv("LEDGER_DB_PATH", "/explicit/ledger.db")
	if p, _ := DefaultPath(); p != "/explicit/ledger.db" {
		t.Errorf("LEDGER_DB_PATH: got %q", p)
	}

	t.Setenv("LEDGER_DB_PATH", "")
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if p, _ := DefaultPath(); p != "/xdg/mcp-local/ledger.db" {
		t.Errorf("XDG_DATA_HOME: got %q", p)
	}

	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/someone")
	if p, _ := DefaultPath(); p != "/home/someone/.local/share/mcp-local/ledger.db" {
		t.Errorf("home fallback: got %q", p)
	}
}

// Pragmas are per connection and database/sql keeps a pool, so they must be
// applied to every pooled connection, not just whichever one served a single
// Exec. Holding three connections at once forces three distinct ones.
func TestOpenConfiguresEveryPooledConnection(t *testing.T) {
	db := openTest(t)
	var conns []*sql.Conn
	for i := 0; i < 3; i++ {
		c, err := db.conn.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		var mode string
		var timeout, fk int
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" || timeout != 5000 || fk != 1 {
			t.Errorf("connection %d: journal_mode=%s busy_timeout=%d foreign_keys=%d; want wal/5000/1", i, mode, timeout, fk)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

// ---------------------------------------------------------------------------
// Golden path: every Store operation, plus the audit trail they leave
// ---------------------------------------------------------------------------

func TestGoldenPathEveryOperation(t *testing.T) {
	db := openTest(t)
	var s Store = db // compile-time: *DB still satisfies the full contract

	// Projects
	p := mustProject(t, db, "alpha")
	again, err := s.GetOrCreateProject(ctx, "alpha", "alpha")
	if err != nil || again.ID != p.ID {
		t.Fatalf("GetOrCreateProject returned a different project: %v %v", again, err)
	}
	if got, err := s.GetProjectByID(ctx, p.ID); err != nil || got.Key != "alpha" {
		t.Fatalf("GetProjectByID: %v %v", got, err)
	}
	projects, err := s.ListProjects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("ListProjects: %v %v", projects, err)
	}

	// Items
	three := 3
	epic, err := s.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Type: "epic", Title: "Parser epic"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, ParentID: epic.ID, Title: "Write the parser",
		Description: "tokenizer first", Priority: &three})
	if err != nil {
		t.Fatal(err)
	}
	if task.Type != "task" || task.Status != "backlog" {
		t.Errorf("CreateItem defaults: type=%s status=%s", task.Type, task.Status)
	}
	if got, err := s.GetItem(ctx, task.ID[:8]); err != nil || got.ID != task.ID {
		t.Errorf("GetItem by prefix: %v %v", got, err)
	}
	if kids, _ := s.ListItems(ctx, ItemFilter{ProjectID: p.ID, ParentID: epic.ID}); len(kids) != 1 || kids[0].ID != task.ID {
		t.Errorf("ListItems by parent: %v", kids)
	}
	if top, _ := s.ListItems(ctx, ItemFilter{ProjectID: p.ID, TopLevelOnly: true}); len(top) != 1 || top[0].ID != epic.ID {
		t.Errorf("ListItems top-level: %v", top)
	}
	if found, _ := s.FindItems(ctx, p.ID, "parser"); len(found) == 0 {
		t.Error("FindItems: no match for 'parser'")
	}
	if hits, _ := s.SearchItems(ctx, p.ID, "tokenizer"); len(hits) != 1 || hits[0].MatchedIn != "description" {
		t.Errorf("SearchItems: %v", hits)
	}

	// UpdateItem changes only what it is given -- the partial-update property
	// the 2026-08 field-erasure incidents were about (U1 in docs/ledger.md).
	upd, err := s.UpdateItem(ctx, task.ID, UpdateItemParams{Label: ptr("core")})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Label.String != "core" || upd.Title != "Write the parser" || upd.Description.String != "tokenizer first" {
		t.Errorf("UpdateItem touched fields it wasn't given: %+v", upd)
	}
	if _, err := s.UpdateItemStatus(ctx, task.ID, "in_progress"); err != nil {
		t.Fatal(err)
	}
	bulk := s.BulkUpdateItemStatus(ctx, []string{epic.ID, task.ID, "no-such-item"}, "planned")
	if len(bulk) != 3 || bulk[0].Error != nil || bulk[1].Error != nil || bulk[2].Error == nil {
		t.Errorf("BulkUpdateItemStatus per-item results wrong: %+v", bulk)
	}
	if it, err := s.UpdateItemPriority(ctx, task.ID, 5); err != nil || it.Priority.Int64 != 5 {
		t.Errorf("UpdateItemPriority: %v %v", it, err)
	}
	if it, err := s.UpdateItemAssignee(ctx, task.ID, "michael"); err != nil || it.Assignee.String != "michael" {
		t.Errorf("UpdateItemAssignee: %v %v", it, err)
	}

	// Resources
	res, err := s.AddResource(ctx, p.ID, task.ID, "https://example.com/spec", "spec")
	if err != nil {
		t.Fatal(err)
	}
	if res.Label.String != "spec" {
		t.Errorf("AddResource returned label %q; want %q (return value used to omit it)", res.Label.String, "spec")
	}
	if list, _ := s.ListResources(ctx, task.ID, ""); len(list) != 1 || list[0].UpdatedAt.Valid {
		t.Errorf("ListResources: want 1 never-updated resource, got %+v", list)
	}
	if r, err := s.UpdateResource(ctx, res.ID, UpdateResourceParams{Label: ptr("design spec")}); err != nil ||
		r.Label.String != "design spec" || r.URL != "https://example.com/spec" || !r.UpdatedAt.Valid {
		t.Errorf("UpdateResource: %+v %v", r, err)
	}

	// Notes and timers
	note, err := s.AddNote(ctx, AddNoteParams{ItemID: task.ID, Body: "first pass done"})
	if err != nil || note.Type != NoteTypeComment {
		t.Fatalf("AddNote: %v %v", note, err)
	}
	if n, err := s.UpdateNote(ctx, note.ID, UpdateNoteParams{Body: ptr("first pass done, tests next")}); err != nil ||
		n.Body.String != "first pass done, tests next" {
		t.Errorf("UpdateNote: %+v %v", n, err)
	}
	if _, err := s.StartTimer(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopTimer(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	notes, _ := s.ListNotes(ctx, task.ID, "")
	var types []string
	for _, n := range notes {
		types = append(types, n.Type)
	}
	if strings.Join(types, ",") != "comment,time-started,time-ended" {
		t.Errorf("ListNotes types = %v", types)
	}

	// Relations
	other := mustItem(t, db, p.ID, "Review the parser")
	if _, err := s.RelateItems(ctx, other.ID, task.ID, "depends_on"); err != nil {
		t.Fatal(err)
	}
	if r := s.BulkRelateItems(ctx, []string{epic.ID}, task.ID, "related_to"); len(r) != 1 || r[0].Error != nil {
		t.Errorf("BulkRelateItems: %+v", r)
	}
	if rels, _ := s.ListRelations(ctx, task.ID); len(rels) != 2 {
		t.Errorf("ListRelations: want 2, got %d", len(rels))
	}
	if rels, _ := s.ListProjectRelations(ctx, p.ID); len(rels) != 2 {
		t.Errorf("ListProjectRelations: want 2, got %d", len(rels))
	}

	// Cross-cutting
	sum, err := s.SummarizeProject(ctx, p.ID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Counts["planned"] != 2 || sum.Counts["backlog"] != 1 || sum.TopPriority == nil || sum.TopPriority.ID != task.ID {
		t.Errorf("SummarizeProject: counts=%v top=%v", sum.Counts, sum.TopPriority)
	}
	if entries, err := s.ListAuditLog(ctx, AuditFilter{EntityID: task.ID}); err != nil || len(entries) == 0 {
		t.Errorf("ListAuditLog: %d entries, %v", len(entries), err)
	}
	if err := s.SoftDelete(ctx, "item", other.ID); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.ListItems(ctx, ItemFilter{ProjectID: p.ID}); len(live) != 2 {
		t.Errorf("soft-deleted item still listed: %d live items", len(live))
	}
	if err := s.Restore(ctx, "item", other.ID); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.ListItems(ctx, ItemFilter{ProjectID: p.ID}); len(live) != 3 {
		t.Errorf("restored item not listed: %d live items", len(live))
	}

	// The audit trail: every mutation above left a row, every row names the
	// client, and every create/update row says what happened. Empty detail
	// on some entity types went unnoticed twice (2026-07-30 resources and
	// relations; 2026-09-10 notes), so this checks all of them together.
	rows, err := db.conn.Query(`SELECT entity_type, operation, COALESCE(detail, ''), COALESCE(client, '') FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var et, op, detail, client string
		if err := rows.Scan(&et, &op, &detail, &client); err != nil {
			t.Fatal(err)
		}
		seen[et+" "+op] = true
		if client != testClient {
			t.Errorf("%s %s: client = %q, want %q", et, op, client, testClient)
		}
		if op != "deleted" && op != "restored" && detail == "" {
			t.Errorf("%s %s: empty audit detail", et, op)
		}
	}
	for _, want := range []string{
		"project created", "item created", "item updated", "item status_changed", "item priority_changed",
		"item assigned", "resource created", "resource updated", "note created", "note updated",
		"item_relation created", "item deleted", "item restored",
	} {
		if !seen[want] {
			t.Errorf("no audit row for %q", want)
		}
	}
}

func TestAuditClientIsNullWithoutResolver(t *testing.T) {
	s, err := Open(Options{Path: filepath.Join(t.TempDir(), "ledger.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.(*DB)
	p := mustProject(t, db, "beta")
	for _, e := range auditRows(t, db, "project", p.ID) {
		if e.Client.Valid {
			t.Errorf("client = %q; want NULL when Options.Client is nil", e.Client.String)
		}
	}
}

// ---------------------------------------------------------------------------
// Vocabulary enforcement
// ---------------------------------------------------------------------------

func TestVocabularyRejectedByValidation(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "gamma")
	a, b := mustItem(t, db, p.ID, "a"), mustItem(t, db, p.ID, "b")

	_, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "x", Type: "bug"})
	wantErrContaining(t, err, `invalid type "bug"`)
	_, err = db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "x", Status: "doing"})
	wantErrContaining(t, err, `invalid status "doing"`)
	_, err = db.UpdateItemStatus(ctx, a.ID, "doing")
	wantErrContaining(t, err, `invalid status "doing"`)
	_, err = db.RelateItems(ctx, a.ID, b.ID, "blocks")
	wantErrContaining(t, err, `invalid relation_type "blocks"`)

	// AddNote writes comments only. Timer events come from the timer
	// functions, which enforce start/stop pairing.
	_, err = db.AddNote(ctx, AddNoteParams{ItemID: a.ID, Type: NoteTypeTimeStarted, Body: "forged"})
	wantErrContaining(t, err, "StartTimer/StopTimer")
	_, err = db.AddNote(ctx, AddNoteParams{ItemID: a.ID, Type: "banana"})
	wantErrContaining(t, err, `invalid note type "banana"`)

	if notes, _ := db.ListNotes(ctx, a.ID, ""); len(notes) != 0 {
		t.Errorf("a rejected AddNote still wrote %d note(s)", len(notes))
	}
	if rels, _ := db.ListRelations(ctx, a.ID); len(rels) != 0 {
		t.Errorf("a rejected RelateItems still wrote %d relation(s)", len(rels))
	}
}

// The triggers must hold even for writes that never touch this package.
func TestVocabularyRejectedByTriggers(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "delta")
	a, b := mustItem(t, db, p.ID, "a"), mustItem(t, db, p.ID, "b")
	now := nowUTC()

	cases := []struct {
		name, sql string
		args      []any
		want      string
	}{
		{"insert item bad status", `INSERT INTO items (id, project_id, type, title, status, created_at, updated_at) VALUES ('i1', ?, 'task', 't', 'doing', ?, ?)`,
			[]any{p.ID, now, now}, "items.status"},
		{"insert item bad type", `INSERT INTO items (id, project_id, type, title, status, created_at, updated_at) VALUES ('i2', ?, 'bug', 't', 'backlog', ?, ?)`,
			[]any{p.ID, now, now}, "items.type"},
		{"update item bad status", `UPDATE items SET status = 'doing' WHERE id = ?`, []any{a.ID}, "items.status"},
		{"update item bad type", `UPDATE items SET type = 'bug' WHERE id = ?`, []any{a.ID}, "items.type"},
		{"insert bad relation", `INSERT INTO item_relations (id, from_item_id, to_item_id, relation_type, created_at) VALUES ('r1', ?, ?, 'serves', ?)`,
			[]any{a.ID, b.ID, now}, "item_relations.relation_type"},
		{"insert bad note type", `INSERT INTO notes (id, item_id, type, created_at, updated_at) VALUES ('n1', ?, 'banana', ?, ?)`,
			[]any{a.ID, now, now}, "notes.type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := db.conn.Exec(c.sql, c.args...)
			wantErrContaining(t, err, "ledger vocabulary: "+c.want)
		})
	}

	// Control: a valid raw write is not blocked.
	if _, err := db.conn.Exec(`INSERT INTO item_relations (id, from_item_id, to_item_id, relation_type, created_at) VALUES ('r2', ?, ?, 'related_to', ?)`,
		a.ID, b.ID, now); err != nil {
		t.Errorf("valid raw relation insert rejected: %v", err)
	}
}

// Rows that predate enforcement are left for a human decision, and must stay
// repairable: soft delete and restore touch only deleted_at, so they must not
// trip the relation_type trigger. Changing the value to another bad one must.
func TestTriggersSparePreexistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db := openAt(t, path)
	p := mustProject(t, db, "epsilon")
	a, b := mustItem(t, db, p.ID, "a"), mustItem(t, db, p.ID, "b")

	// Recreate the real situation: a bad row written before the triggers
	// existed.
	if _, err := db.conn.Exec(`DROP TRIGGER ` + relationTriggerName("ins")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO item_relations (id, from_item_id, to_item_id, relation_type, created_at) VALUES ('legacy', ?, ?, 'blocks', ?)`,
		a.ID, b.ID, nowUTC()); err != nil {
		t.Fatal(err)
	}

	// Reopen: the missing trigger is reinstalled.
	db2 := openAt(t, path)
	var n int
	db2.conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name = ?`, relationTriggerName("ins")).Scan(&n)
	if n != 1 {
		t.Fatalf("relation insert trigger not reinstalled on reopen")
	}

	_, err := db2.conn.Exec(`UPDATE item_relations SET relation_type = 'serves' WHERE id = 'legacy'`)
	wantErrContaining(t, err, "ledger vocabulary: item_relations.relation_type")

	if err := db2.SoftDelete(ctx, "item_relation", "legacy"); err != nil {
		t.Errorf("soft-deleting a pre-existing off-vocabulary row was blocked: %v", err)
	}
	if err := db2.Restore(ctx, "item_relation", "legacy"); err != nil {
		t.Errorf("restoring a pre-existing off-vocabulary row was blocked: %v", err)
	}
	if _, err := db2.conn.Exec(`UPDATE item_relations SET relation_type = 'blocked_by' WHERE id = 'legacy'`); err != nil {
		t.Errorf("repairing a pre-existing row to a valid value was blocked: %v", err)
	}
}

func relationTriggerName(kind string) string {
	for _, r := range vocabRules() {
		if r.table == "item_relations" {
			return r.triggerPrefix() + kind + "_" + r.suffix()
		}
	}
	return ""
}

// A vocabulary change must replace the installed triggers. If an outdated
// trigger survived, it would reject every value the new vocabulary added.
func TestStaleVocabularyTriggerIsReplaced(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "zeta")
	stale := "ledger_vocab_items_status_ins_deadbeef"
	if _, err := db.conn.Exec(`CREATE TRIGGER ` + stale + ` BEFORE INSERT ON items WHEN NEW.status NOT IN ('backlog')
		BEGIN SELECT RAISE(ABORT, 'stale vocabulary'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "x", Status: "done"})
	wantErrContaining(t, err, "stale vocabulary")

	if err := ensureVocabTriggers(db.conn); err != nil {
		t.Fatal(err)
	}
	var n int
	db.conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name = ?`, stale).Scan(&n)
	if n != 0 {
		t.Errorf("stale trigger %s was not dropped", stale)
	}
	if _, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "x", Status: "done"}); err != nil {
		t.Errorf("valid status still rejected after replacing the stale trigger: %v", err)
	}
}

func TestVocabReturnsCopies(t *testing.T) {
	v := Vocab()
	v.Statuses[0] = "mutated"
	if Vocab().Statuses[0] != "backlog" {
		t.Error("Vocab() exposed the package's own slice")
	}
}

// ---------------------------------------------------------------------------
// Editing notes and resources
// ---------------------------------------------------------------------------

func TestUpdateNote(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "eta")
	it := mustItem(t, db, p.ID, "a")
	note, err := db.AddNote(ctx, AddNoteParams{ItemID: it.ID, Body: "draft", URL: "https://x.test"})
	if err != nil {
		t.Fatal(err)
	}

	// Unchanged value: no write, no audit row.
	if _, err := db.UpdateNote(ctx, note.ID, UpdateNoteParams{Body: ptr("draft")}); err != nil {
		t.Fatal(err)
	}
	if rows := auditRows(t, db, "note", note.ID); len(rows) != 1 {
		t.Errorf("no-op update wrote an audit row: %d rows", len(rows))
	}

	// Clearing a field with "" is recorded as old -> new.
	n, err := db.UpdateNote(ctx, note.ID, UpdateNoteParams{URL: ptr("")})
	if err != nil || n.URL.Valid || n.Body.String != "draft" {
		t.Fatalf("clear url: %+v %v", n, err)
	}
	rows := auditRows(t, db, "note", note.ID)
	var detail map[string][2]string
	if err := json.Unmarshal([]byte(rows[len(rows)-1].Detail.String), &detail); err != nil ||
		detail["url"] != [2]string{"https://x.test", ""} {
		t.Errorf("update audit detail = %q", rows[len(rows)-1].Detail.String)
	}

	_, err = db.UpdateNote(ctx, note.ID, UpdateNoteParams{})
	wantErrContaining(t, err, "at least one")

	timer, err := db.StartTimer(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.UpdateNote(ctx, timer.ID, UpdateNoteParams{Body: ptr("rewritten history")})
	wantErrContaining(t, err, "timer event")

	if err := db.SoftDelete(ctx, "note", note.ID); err != nil {
		t.Fatal(err)
	}
	_, err = db.UpdateNote(ctx, note.ID, UpdateNoteParams{Body: ptr("x")})
	wantErrContaining(t, err, "restore it first")

	_, err = db.UpdateNote(ctx, "no-such-note", UpdateNoteParams{Body: ptr("x")})
	wantErrContaining(t, err, "not found")
}

func TestUpdateResource(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "theta")
	it := mustItem(t, db, p.ID, "a")
	r, err := db.AddResource(ctx, p.ID, it.ID, "https://a.test", "")
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.UpdateResource(ctx, r.ID, UpdateResourceParams{URL: ptr("")})
	wantErrContaining(t, err, "cannot be cleared")

	got, err := db.UpdateResource(ctx, r.ID, UpdateResourceParams{URL: ptr("https://b.test"), Label: ptr("docs")})
	if err != nil || got.URL != "https://b.test" || got.Label.String != "docs" {
		t.Fatalf("UpdateResource: %+v %v", got, err)
	}
	rows := auditRows(t, db, "resource", r.ID)
	var detail map[string][2]string
	json.Unmarshal([]byte(rows[len(rows)-1].Detail.String), &detail)
	if detail["url"] != [2]string{"https://a.test", "https://b.test"} || detail["label"] != [2]string{"", "docs"} {
		t.Errorf("update audit detail = %q", rows[len(rows)-1].Detail.String)
	}

	if err := db.SoftDelete(ctx, "resource", r.ID); err != nil {
		t.Fatal(err)
	}
	_, err = db.UpdateResource(ctx, r.ID, UpdateResourceParams{Label: ptr("x")})
	wantErrContaining(t, err, "restore it first")
}

// A database created before resources.updated_at existed gains the column on
// open, and its existing rows are left NULL -- not backfilled. Adding a column
// must not rewrite data.
func TestResourcesUpdatedAtMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE projects (id TEXT PRIMARY KEY, key TEXT UNIQUE NOT NULL, name TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE resources (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id), item_id TEXT, url TEXT NOT NULL, label TEXT, created_at TEXT NOT NULL)`,
		`INSERT INTO projects VALUES ('p1', 'old', 'old', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO resources VALUES ('r1', 'p1', NULL, 'https://old.test', 'old', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	raw.Close()

	db := openAt(t, path)
	list, err := db.ListResources(ctx, "", "p1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListResources after migration: %v %v", list, err)
	}
	if list[0].UpdatedAt.Valid {
		t.Errorf("existing row was backfilled: updated_at = %q", list[0].UpdatedAt.String)
	}
	if list[0].URL != "https://old.test" || list[0].Label.String != "old" || list[0].CreatedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("existing row changed by migration: %+v", list[0])
	}
}

// ---------------------------------------------------------------------------
// Concurrency across processes
// ---------------------------------------------------------------------------

// Regression test for 2026-09-07: with no busy timeout, 923 of 1000 writes
// from 5 concurrent processes failed with SQLITE_BUSY. Each writer does both
// shapes of transaction: a create, and an update that reads the row before
// writing it. The second shape needs BEGIN IMMEDIATE as well as the busy
// timeout (see store.go) and went untested until 2026-09-10. This mirrors how
// mcp-local actually uses the store -- a fresh Open, one write and a Close
// per tool call -- across separate OS processes, because SQLite file locking
// between processes is the thing under test and goroutines in one process
// would not exercise it.
func TestConcurrentWritersAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	const procs, perProc = 5, 100
	path := filepath.Join(t.TempDir(), "ledger.db")

	// The parent opens first, as a deploy does, so the children run against
	// an initialised WAL database -- the steady state every session sees.
	parent := openAt(t, path)
	p := mustProject(t, parent, "concurrency")

	cmds := make([]*exec.Cmd, procs)
	outs := make([]*strings.Builder, procs)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(),
			"LEDGERCORE_WRITER=w"+strconv.Itoa(i),
			"LEDGERCORE_PATH="+path,
			"LEDGERCORE_PROJECT="+p.ID,
			"LEDGERCORE_N="+strconv.Itoa(perProc),
		)
		outs[i] = &strings.Builder{}
		cmd.Stdout, cmd.Stderr = outs[i], outs[i]
		cmds[i] = cmd
	}
	for _, c := range cmds {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Errorf("writer %d failed (%v):\n%s", i, err, outs[i].String())
		}
	}

	var items, done, audited int
	parent.conn.QueryRow(`SELECT COUNT(*) FROM items WHERE project_id = ?`, p.ID).Scan(&items)
	parent.conn.QueryRow(`SELECT COUNT(*) FROM items WHERE project_id = ? AND status = 'done'`, p.ID).Scan(&done)
	parent.conn.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE entity_type = 'item' AND client LIKE 'writer/%'`).Scan(&audited)
	if items != procs*perProc || done != procs*perProc || audited != 2*procs*perProc {
		t.Errorf("created %d, updated %d, audited %d; want %d, %d, %d",
			items, done, audited, procs*perProc, procs*perProc, 2*procs*perProc)
	}
}

// runWriter is the child-process side of TestConcurrentWritersAcrossProcesses.
func runWriter() int {
	worker := os.Getenv("LEDGERCORE_WRITER")
	path, project := os.Getenv("LEDGERCORE_PATH"), os.Getenv("LEDGERCORE_PROJECT")
	n, _ := strconv.Atoi(os.Getenv("LEDGERCORE_N"))
	opts := Options{Path: path, Actor: func(context.Context) Actor {
		return Actor{Kind: ActorProgram, ID: "writer/" + worker, Label: "writer/" + worker}
	}}

	failures := 0
	for i := 0; i < n; i++ {
		s, err := Open(opts)
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "open %d: %v\n", i, err)
			continue
		}
		it, err := s.CreateItem(context.Background(), CreateItemParams{ProjectID: project, Title: fmt.Sprintf("%s-%d", worker, i)})
		s.Close()
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "create %d: %v\n", i, err)
			continue
		}
		// A separate open, as a second tool call would be: read-then-write.
		s, err = Open(opts)
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "open for update %d: %v\n", i, err)
			continue
		}
		if _, err := s.UpdateItemStatus(context.Background(), it.ID, "done"); err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "update %d: %v\n", i, err)
		}
		s.Close()
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "%s: %d of %d writes failed\n", worker, failures, n)
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Soft-deleted entities are read-only until restored
// ---------------------------------------------------------------------------

// Decided 2026-09-10 (defect b1c028d7): a soft-deleted item accepts no
// changes except Restore. Before this, a deleted item -- hidden from every
// list -- still took status, priority, assignee and field edits.
func TestDeletedItemIsReadOnlyUntilRestored(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "iota")
	it := mustItem(t, db, p.ID, "to be deleted")
	if err := db.SoftDelete(ctx, "item", it.ID); err != nil {
		t.Fatal(err)
	}
	auditBefore := len(auditRows(t, db, "item", it.ID))

	const want = "is deleted; restore it first (ledger_restore entity_type=item"
	_, err := db.UpdateItem(ctx, it.ID, UpdateItemParams{Title: ptr("edited while deleted")})
	wantErrContaining(t, err, want)
	_, err = db.UpdateItemStatus(ctx, it.ID, "done")
	wantErrContaining(t, err, want)
	_, err = db.UpdateItemPriority(ctx, it.ID, 9)
	wantErrContaining(t, err, want)
	_, err = db.UpdateItemAssignee(ctx, it.ID, "someone")
	wantErrContaining(t, err, want)
	bulk := db.BulkUpdateItemStatus(ctx, []string{it.ID}, "done")
	if len(bulk) != 1 || bulk[0].Error == nil || !strings.Contains(bulk[0].Error.Error(), want) {
		t.Errorf("BulkUpdateItemStatus on a deleted item: %+v", bulk)
	}

	// Nothing changed, and nothing was audited as if it had.
	var title, status string
	var priority, assignee sql.NullString
	db.conn.QueryRow(`SELECT title, status, priority, assignee FROM items WHERE id = ?`, it.ID).Scan(&title, &status, &priority, &assignee)
	if title != "to be deleted" || status != "backlog" || priority.Valid || assignee.Valid {
		t.Errorf("deleted item was modified: title=%q status=%q priority=%v assignee=%v", title, status, priority, assignee)
	}
	if n := len(auditRows(t, db, "item", it.ID)); n != auditBefore {
		t.Errorf("refused edits wrote %d audit row(s)", n-auditBefore)
	}

	// Restore is the one permitted change, and it makes the item editable again.
	if err := db.Restore(ctx, "item", it.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateItemStatus(ctx, it.ID, "done"); err != nil {
		t.Errorf("restored item still refused edits: %v", err)
	}
}

// Extended 2026-09-10: attaching anything to a deleted item edits it, so every
// attach path is refused too -- and the same for a deleted project. Cleaning
// up (soft-deleting a relation that points at a deleted item) must still work.
func TestAttachingToDeletedEntitiesIsRefused(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "kappa")
	live, gone := mustItem(t, db, p.ID, "live"), mustItem(t, db, p.ID, "gone")
	oldRel, err := db.RelateItems(ctx, live.ID, gone.ID, "related_to")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartTimer(ctx, gone.ID); err != nil { // a timer left running at delete time
		t.Fatal(err)
	}
	if err := db.SoftDelete(ctx, "item", gone.ID); err != nil {
		t.Fatal(err)
	}

	const want = "is deleted; restore it first (ledger_restore entity_type=item"
	_, err = db.AddNote(ctx, AddNoteParams{ItemID: gone.ID, Body: "x"})
	wantErrContaining(t, err, want)
	_, err = db.AddResource(ctx, p.ID, gone.ID, "https://x.test", "")
	wantErrContaining(t, err, want)
	_, err = db.StartTimer(ctx, gone.ID)
	wantErrContaining(t, err, want) // not "timer already running"
	_, err = db.StopTimer(ctx, gone.ID)
	wantErrContaining(t, err, want)
	_, err = db.RelateItems(ctx, live.ID, gone.ID, "depends_on")
	wantErrContaining(t, err, want)
	_, err = db.RelateItems(ctx, gone.ID, live.ID, "depends_on")
	wantErrContaining(t, err, want)
	_, err = db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, ParentID: gone.ID, Title: "child"})
	wantErrContaining(t, err, want)

	var notes, resources, relations, items int
	db.conn.QueryRow(`SELECT COUNT(*) FROM notes WHERE item_id = ?`, gone.ID).Scan(&notes)
	db.conn.QueryRow(`SELECT COUNT(*) FROM resources WHERE item_id = ?`, gone.ID).Scan(&resources)
	db.conn.QueryRow(`SELECT COUNT(*) FROM item_relations WHERE from_item_id = ? OR to_item_id = ?`, gone.ID, gone.ID).Scan(&relations)
	db.conn.QueryRow(`SELECT COUNT(*) FROM items WHERE parent_id = ?`, gone.ID).Scan(&items)
	if notes != 1 || resources != 0 || relations != 1 || items != 0 {
		t.Errorf("refused writes left rows behind: notes=%d (want the 1 pre-delete timer) resources=%d relations=%d children=%d",
			notes, resources, relations, items)
	}

	// Cleanup is still possible: an existing relation to a deleted item can be
	// soft-deleted, and so can the leftover itself be restored.
	if err := db.SoftDelete(ctx, "item_relation", oldRel.ID); err != nil {
		t.Errorf("soft-deleting a relation to a deleted item was blocked: %v", err)
	}

	// Restore re-enables every attach path.
	if err := db.Restore(ctx, "item", gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.StopTimer(ctx, gone.ID); err != nil {
		t.Errorf("restored item: StopTimer: %v", err)
	}
	if _, err := db.AddNote(ctx, AddNoteParams{ItemID: gone.ID, Body: "back"}); err != nil {
		t.Errorf("restored item: AddNote: %v", err)
	}

	// Deleted project: nothing new can be added to it.
	q := mustProject(t, db, "lambda")
	if err := db.SoftDelete(ctx, "project", q.ID); err != nil {
		t.Fatal(err)
	}
	const wantProject = "is deleted; restore it first (ledger_restore entity_type=project"
	_, err = db.CreateItem(ctx, CreateItemParams{ProjectID: q.ID, Title: "x"})
	wantErrContaining(t, err, wantProject)
	_, err = db.AddNote(ctx, AddNoteParams{ProjectID: q.ID, Body: "x"})
	wantErrContaining(t, err, wantProject)
	_, err = db.AddResource(ctx, q.ID, "", "https://x.test", "")
	wantErrContaining(t, err, wantProject)
}

// ---------------------------------------------------------------------------
// What ledger-server needs from reads (Phase 1a)
// ---------------------------------------------------------------------------

func TestGetItemReportsDeletedAndClassifiesLookupFailures(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "mu")
	it := mustItem(t, db, p.ID, "x")

	got, _ := db.GetItem(ctx, it.ID)
	if got.DeletedAt.Valid {
		t.Error("live item reports DeletedAt")
	}
	db.SoftDelete(ctx, "item", it.ID)
	got, err := db.GetItem(ctx, it.ID)
	if err != nil || !got.DeletedAt.Valid {
		t.Errorf("GetItem on a deleted item: DeletedAt=%v err=%v; want it returned, marked deleted", got, err)
	}
	if live, _ := db.ListItems(ctx, ItemFilter{ProjectID: p.ID}); len(live) != 0 {
		t.Error("ListItems returned a deleted item")
	}

	_, err = db.GetItem(ctx, "ffffffff-no-such-item")
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrAmbiguousID) {
		t.Errorf("not-found lookup: %v", err)
	}
	if err.Error() != `item "ffffffff-no-such-item" not found` {
		t.Errorf("not-found message changed: %q", err.Error())
	}

	// Force an ambiguous prefix: two ids sharing a first character is
	// guaranteed across 17 items (16 hex digits, pigeonhole).
	seen := map[byte]bool{}
	var shared string
	for i := 0; shared == "" && i < 17; i++ {
		c := mustItem(t, db, p.ID, fmt.Sprintf("m%d", i)).ID[0]
		if seen[c] {
			shared = string(c)
		}
		seen[c] = true
	}
	_, err = db.GetItem(ctx, shared)
	if !errors.Is(err, ErrAmbiguousID) || !strings.Contains(err.Error(), "matches more than one item") {
		t.Errorf("ambiguous lookup: %v", err)
	}
}

func TestItemURL(t *testing.T) {
	t.Setenv("LEDGER_SERVER_URL", "")
	if got := ItemURL("abc"); got != "http://127.0.0.1:8090/t/abc" {
		t.Errorf("default: %q", got)
	}
	t.Setenv("LEDGER_SERVER_URL", "http://ledger.local:9000/ ")
	if got := ItemURL("abc"); got != "http://ledger.local:9000/t/abc" {
		t.Errorf("override: %q", got)
	}
}

// Search is "contains", with * and ? as the wildcards people type, and % and
// _ treated literally -- SQLite's LIKE would otherwise make "50%" match
// everything after "50". (2026-09-11, from review feedback that search did
// not do text or wildcard matching.)
func TestSearchPatterns(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "search")
	mk := func(title, desc string) string {
		it, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: title, Description: desc})
		if err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	parser := mk("Write the parser", "tokenizer first")
	report := mk("Coverage at 50% of statements", "")
	noted := mk("Unrelated", "")
	if _, err := db.AddNote(ctx, AddNoteParams{ItemID: noted, Body: "the parser needs a lexer"}); err != nil {
		t.Fatal(err)
	}

	ids := func(q string) []string {
		t.Helper()
		res, err := db.SearchItems(ctx, p.ID, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range res {
			out = append(out, r.ItemID+":"+r.MatchedIn)
		}
		return out
	}
	has := func(got []string, want string) bool {
		for _, g := range got {
			if g == want {
				return true
			}
		}
		return false
	}

	if got := ids("parser"); !has(got, parser+":title") || !has(got, noted+":note") || len(got) != 2 {
		t.Errorf("plain substring: %v", got)
	}
	if got := ids("tokenizer"); !has(got, parser+":description") {
		t.Errorf("description match: %v", got)
	}
	if got := ids("Write*parser"); !has(got, parser+":title") {
		t.Errorf("* wildcard: %v", got)
	}
	if got := ids("part?"); len(got) != 0 {
		t.Errorf("? matches exactly one character, so 'part?' shouldn't match 'parser': %v", got)
	}
	if got := ids("parse?"); !has(got, parser+":title") {
		t.Errorf("? wildcard: %v", got)
	}
	// The bug this guards: '%' as a literal, not "match anything".
	if got := ids("50%"); !has(got, report+":title") || len(got) != 1 {
		t.Errorf("literal %%: %v", got)
	}
	if got := ids("50% of"); !has(got, report+":title") {
		t.Errorf("literal %% mid-string: %v", got)
	}
	// FindItems (titles only) shares the pattern rules.
	found, err := db.FindItems(ctx, p.ID, "Write*parser")
	if err != nil || len(found) != 1 || found[0].ID != parser {
		t.Errorf("FindItems wildcard: %v %v", found, err)
	}
}

// The ledger's own linter. Each case below is a problem that actually
// occurred in the live database at some point.
func TestHealthFindings(t *testing.T) {
	db := openTest(t)
	byCheck := func() map[string][]Finding {
		t.Helper()
		found, err := db.HealthFindings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]Finding{}
		for _, f := range found {
			out[f.Check] = append(out[f.Check], f)
		}
		return out
	}

	// A healthy ledger reports nothing.
	good := mustProject(t, db, "good")
	keeper := mustItem(t, db, good.ID, "keeper")
	if got := byCheck(); len(got) != 0 {
		t.Fatalf("clean ledger reported %v", got)
	}

	// 1. An empty project.
	empty := mustProject(t, db, "project=oops")
	// 2. A live ticket whose project is deleted.
	gone := mustProject(t, db, "gone")
	stranded := mustItem(t, db, gone.ID, "stranded")
	if err := db.SoftDelete(ctx, "project", gone.ID); err != nil {
		t.Fatal(err)
	}
	// 3. A live ticket under a deleted parent.
	parent := mustItem(t, db, good.ID, "parent")
	child, err := db.CreateItem(ctx, CreateItemParams{ProjectID: good.ID, ParentID: parent.ID, Title: "child"})
	if err != nil {
		t.Fatal(err)
	}
	// 4. A relation left pointing at a deleted ticket.
	rel, err := db.RelateItems(ctx, keeper.ID, parent.ID, "related_to")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SoftDelete(ctx, "item", parent.ID); err != nil {
		t.Fatal(err)
	}
	// 5. An off-vocabulary value from before enforcement: drop the trigger,
	//    write one, and put the trigger back.
	if _, err := db.conn.Exec(`DROP TRIGGER ` + relationTriggerName("ins")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`INSERT INTO item_relations (id, from_item_id, to_item_id, relation_type, created_at)
		VALUES ('legacy', ?, ?, 'serves', ?)`, keeper.ID, child.ID, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if err := ensureVocabTriggers(db.conn); err != nil {
		t.Fatal(err)
	}
	// 6. A timer nobody stopped.
	if _, err := db.StartTimer(ctx, keeper.ID); err != nil {
		t.Fatal(err)
	}
	// 7. A component name that no longer resolves.
	if _, err := db.CreateItem(ctx, CreateItemParams{ProjectID: good.ID, Type: "component", Title: "auth"}); err != nil {
		t.Fatal(err)
	}
	orphanComponent, err := db.CreateItem(ctx, CreateItemParams{ProjectID: good.ID, Title: "uses auth", Component: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	var componentID string
	db.conn.QueryRow(`SELECT id FROM items WHERE type='component' AND title='auth'`).Scan(&componentID)
	if err := db.SoftDelete(ctx, "item", componentID); err != nil {
		t.Fatal(err)
	}

	got := byCheck()
	for _, c := range []struct {
		check, entity string
		want          int
	}{
		{"empty_project", empty.ID, 1},
		{"item_in_deleted_project", stranded.ID, 1},
		{"deleted_parent", child.ID, 1},
		{"dangling_relation", rel.ID, 1},
		{"off_vocabulary", "legacy", 1},
		{"running_timer", keeper.ID, 1},
		{"missing_component", orphanComponent.ID, 1},
	} {
		findings := got[c.check]
		if len(findings) != c.want {
			t.Errorf("%s: %d findings, want %d (%+v)", c.check, len(findings), c.want, findings)
			continue
		}
		if findings[0].EntityID != c.entity {
			t.Errorf("%s: reported %s, want %s", c.check, findings[0].EntityID, c.entity)
		}
		if findings[0].Detail == "" || findings[0].Title == "" {
			t.Errorf("%s: finding needs a title and an explanation: %+v", c.check, findings[0])
		}
		// Everything except an off-vocabulary value has one exact fix; that
		// one needs a person to decide what it meant.
		if (c.check == "off_vocabulary") != (findings[0].Fix == "") {
			t.Errorf("%s: Fix = %q", c.check, findings[0].Fix)
		}
	}

	// Reporting is read-only.
	before, _ := db.ListAuditLog(ctx, AuditFilter{})
	db.HealthFindings(ctx)
	after, _ := db.ListAuditLog(ctx, AuditFilter{})
	if len(after) != len(before) {
		t.Errorf("running the checks wrote %d audit row(s)", len(after)-len(before))
	}

	// Acting on a finding clears it.
	if _, err := db.StopTimer(ctx, keeper.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SoftDelete(ctx, "item_relation", rel.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SoftDelete(ctx, "project", empty.ID); err != nil {
		t.Fatal(err)
	}
	got = byCheck()
	for _, check := range []string{"running_timer", "dangling_relation", "empty_project"} {
		if len(got[check]) != 0 {
			t.Errorf("%s still reported after it was fixed: %+v", check, got[check])
		}
	}
}

// ---------------------------------------------------------------------------
// Who did it, as one action, and what may be refused (2026-09-12)
// ---------------------------------------------------------------------------

func openAs(t *testing.T, path string, actor Actor, permit func(context.Context, Actor, Operation, Target) error) *DB {
	t.Helper()
	s, err := Open(Options{
		Path:   path,
		Actor:  func(context.Context) Actor { return actor },
		Permit: permit,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s.(*DB)
}

func TestActorAndAuthorRecorded(t *testing.T) {
	person := Actor{Kind: ActorPerson, ID: "u-1", Label: "Michael"}
	db := openAs(t, filepath.Join(t.TempDir(), "l.db"), person, nil)
	p := mustProject(t, db, "actors")
	it := mustItem(t, db, p.ID, "a ticket")

	// Every audit row carries both forms: the label a person reads, and the
	// canonical id a future account system can resolve.
	for _, e := range auditRows(t, db, "item", it.ID) {
		if e.Client.String != "Michael" || e.Actor.String != "person:u-1" {
			t.Errorf("audit row: client=%q actor=%q", e.Client.String, e.Actor.String)
		}
		if e.Batch.Valid {
			t.Errorf("a single edit should not be part of a batch: %q", e.Batch.String)
		}
	}

	// A comment records its author, which is what "who wrote this" needs.
	note, err := db.AddNote(ctx, AddNoteParams{ItemID: it.ID, Body: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	if note.Author.String != "person:u-1" || note.AuthorLabel.String != "Michael" {
		t.Errorf("note author: %q / %q", note.Author.String, note.AuthorLabel.String)
	}
	notes, _ := db.ListNotes(ctx, it.ID, "")
	if len(notes) != 1 || notes[0].Author.String != "person:u-1" {
		t.Errorf("author not read back: %+v", notes)
	}

	// No resolver: NULL, not a guess.
	anon := openAs(t, filepath.Join(t.TempDir(), "anon.db"), Actor{}, nil)
	ap := mustProject(t, anon, "anon")
	for _, e := range auditRows(t, anon, "project", ap.ID) {
		if e.Actor.Valid || e.Client.Valid {
			t.Errorf("unknown actor recorded as %q/%q", e.Client.String, e.Actor.String)
		}
	}
}

// A bulk change was N unrelated audit rows until now. One batch id makes it
// one event with N parts -- which is what makes it reportable and undoable.
func TestBulkChangeIsOneAuditableAction(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "bulk")
	a, b, c := mustItem(t, db, p.ID, "a"), mustItem(t, db, p.ID, "b"), mustItem(t, db, p.ID, "c")

	results := db.BulkUpdateItemStatus(ctx, []string{a.ID, b.ID, c.ID, "no-such-item"}, "done")
	if len(results) != 4 || results[3].Error == nil {
		t.Fatalf("expected three successes and one failure: %+v", results)
	}

	batches := map[string]int{}
	for _, id := range []string{a.ID, b.ID, c.ID} {
		rows := auditRows(t, db, "item", id)
		last := rows[len(rows)-1]
		if last.Operation != "status_changed" || !last.Batch.Valid {
			t.Fatalf("%s: operation=%s batch=%v", id, last.Operation, last.Batch)
		}
		batches[last.Batch.String]++
	}
	if len(batches) != 1 {
		t.Errorf("the three changes should share one batch id, got %v", batches)
	}

	// A single edit afterwards is not part of that batch.
	db.UpdateItemStatus(ctx, a.ID, "planned")
	rows := auditRows(t, db, "item", a.ID)
	if rows[len(rows)-1].Batch.Valid {
		t.Error("a single edit was tagged with a batch id")
	}
}

func TestPrivilegedOperationsPassThroughPolicy(t *testing.T) {
	var asked []Operation
	refuse := errors.New("not allowed here")
	deny := func(_ context.Context, _ Actor, op Operation, target Target) error {
		asked = append(asked, op)
		if op == OpDeleteProject || op == OpBulkUpdateStatus {
			return refuse
		}
		if op == OpDeleteProject && target.ProjectID == "" {
			t.Error("a project operation must name its project, for per-project access later")
		}
		return nil
	}
	db := openAs(t, filepath.Join(t.TempDir(), "l.db"), Actor{Kind: ActorPerson, ID: "u-1"}, deny)
	p := mustProject(t, db, "policy")
	it := mustItem(t, db, p.ID, "a ticket")

	// Refused, and nothing happened.
	if err := db.SoftDelete(ctx, "project", p.ID); !errors.Is(err, refuse) {
		t.Errorf("deleting a project: %v", err)
	}
	if projects, _ := db.ListProjects(ctx); len(projects) != 1 {
		t.Error("the project was deleted despite the refusal")
	}
	for _, r := range db.BulkUpdateItemStatus(ctx, []string{it.ID}, "done") {
		if !errors.Is(r.Error, refuse) {
			t.Errorf("bulk: %v", r.Error)
		}
	}
	if got, _ := db.GetItem(ctx, it.ID); got.Status != "backlog" {
		t.Errorf("bulk change applied despite the refusal: %s", got.Status)
	}

	// Ordinary work is not gated at all.
	if _, err := db.UpdateItemStatus(ctx, it.ID, "planned"); err != nil {
		t.Errorf("an ordinary status change was refused: %v", err)
	}
	if err := db.SoftDelete(ctx, "item", it.ID); err != nil {
		t.Errorf("deleting one's own ticket was refused: %v", err)
	}
	for _, op := range asked {
		if op != OpDeleteProject && op != OpBulkUpdateStatus {
			t.Errorf("policy asked about an operation that should be ordinary: %s", op)
		}
	}
}

// Editing or deleting someone else's comment is privileged; your own is not.
// A comment with no recorded author (everything written before 2026-09-12)
// counts as yours, so history doesn't lock the operator out.
func TestOthersCommentsArePrivileged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	alice := Actor{Kind: ActorPerson, ID: "alice"}
	bob := Actor{Kind: ActorPerson, ID: "bob"}

	var asked []Operation
	record := func(_ context.Context, _ Actor, op Operation, _ Target) error {
		asked = append(asked, op)
		return nil
	}

	dbA := openAs(t, path, alice, record)
	p := mustProject(t, dbA, "comments")
	it := mustItem(t, dbA, p.ID, "a ticket")
	hers, err := dbA.AddNote(ctx, AddNoteParams{ItemID: it.ID, Body: "alice wrote this"})
	if err != nil {
		t.Fatal(err)
	}
	// A pre-authorship note: author NULL, as every existing note is.
	legacy, _ := dbA.AddNote(ctx, AddNoteParams{ItemID: it.ID, Body: "from before"})
	if _, err := dbA.conn.Exec(`UPDATE notes SET author = NULL, author_label = NULL WHERE id = ?`, legacy.ID); err != nil {
		t.Fatal(err)
	}

	// Alice deleting her own note asks nothing.
	asked = nil
	if err := dbA.SoftDelete(ctx, "note", hers.ID); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Errorf("deleting one's own comment was gated: %v", asked)
	}
	// Nor does anyone deleting an unattributed one.
	if err := dbA.SoftDelete(ctx, "note", legacy.ID); err != nil || len(asked) != 0 {
		t.Errorf("an unattributed comment was gated: %v %v", err, asked)
	}

	// Bob deleting Alice's note does go through the policy.
	dbB := openAs(t, path, bob, record)
	other, err := dbB.AddNote(ctx, AddNoteParams{ItemID: it.ID, Body: "bob wrote this"})
	if err != nil {
		t.Fatal(err)
	}
	asked = nil
	if err := dbA.SoftDelete(ctx, "note", other.ID); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != OpDeleteOthersNote {
		t.Errorf("deleting another actor's comment should be privileged, asked: %v", asked)
	}
}
