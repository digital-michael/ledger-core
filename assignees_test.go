package ledgercore

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Assignees and watchers (ledger story 13ed93aa): the same shape as labels --
// child tables, unique per live member, soft delete, audited -- holding identities.

func assigneesOf(t *testing.T, db *DB, id string) []string {
	t.Helper()
	got, err := db.GetItem(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignees == nil {
		return []string{}
	}
	return got.Assignees
}

func watchersOf(t *testing.T, db *DB, id string) []string {
	t.Helper()
	got, err := db.GetItem(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Watchers == nil {
		return []string{}
	}
	return got.Watchers
}

func plainDB(t *testing.T) (*DB, *Project) {
	t.Helper()
	db := openAs(t, filepath.Join(t.TempDir(), "l.db"), script, nil) // a program: no default assignee or owner
	return db, mustProject(t, db, "demo")
}

func TestAssignees_addRemoveListAndUnique(t *testing.T) {
	db, p := plainDB(t)
	it := mustItem(t, db, p.ID, "t")
	before, _ := db.GetItem(ctx, it.ID)

	row, added, err := db.AddAssignee(ctx, it.ID, " Person:Alice ")
	if err != nil || !added || row.Member != "person:alice" {
		t.Fatalf("add: %+v %v %v", row, added, err)
	}
	for _, dup := range []string{"person:alice", "PERSON:ALICE", " person:alice "} {
		again, added, err := db.AddAssignee(ctx, it.ID, dup)
		if err != nil || added || again.ID != row.ID {
			t.Errorf("adding %q again should be a no-op: %v %v", dup, added, err)
		}
	}
	if _, _, err := db.AddAssignee(ctx, it.ID, "agent:claude-code"); err != nil {
		t.Fatal(err)
	}
	if got := assigneesOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"agent:claude-code", "person:alice"}) {
		t.Errorf("assignees = %v", got)
	}
	// The compatibility view is the joined list.
	if cur, _ := db.GetItem(ctx, it.ID); cur.Assignee.String != "agent:claude-code, person:alice" || cur.UpdatedAt == before.UpdatedAt {
		t.Errorf("compat view / updated_at: %q %v", cur.Assignee.String, cur.UpdatedAt == before.UpdatedAt)
	}
	list, _ := db.ListAssignees(ctx, it.ID)
	if len(list) != 2 || list[0].Member != "agent:claude-code" {
		t.Errorf("list: %+v", list)
	}

	removed, err := db.RemoveAssignee(ctx, it.ID, "PERSON:alice")
	if err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if removed, err := db.RemoveAssignee(ctx, it.ID, "person:alice"); err != nil || removed {
		t.Errorf("removing an absent assignee: %v %v", removed, err)
	}
	// A removed assignee can come back.
	if _, added, err := db.AddAssignee(ctx, it.ID, "person:alice"); err != nil || !added {
		t.Errorf("re-add: %v %v", added, err)
	}
}

func TestAssignees_onlyPeopleAndAgentsAndStrictForm(t *testing.T) {
	db, p := plainDB(t)
	it := mustItem(t, db, p.ID, "t")
	for _, bad := range []string{"", "Michael", "digital-michael", "program:mcp", "robot:x", "person:", "person:a b"} {
		if _, _, err := db.AddAssignee(ctx, it.ID, bad); err == nil {
			t.Errorf("assignee %q accepted", bad)
		}
		if _, _, err := db.AddWatcher(ctx, it.ID, bad); err == nil {
			t.Errorf("watcher %q accepted", bad)
		}
	}
	if _, _, err := db.AddAssignee(ctx, "nope", "person:alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown item: %v", err)
	}
	if _, _, err := db.AddWatcher(ctx, it.ID, "agent:claude-code"); err != nil {
		t.Errorf("an agent may watch: %v", err)
	}
}

func TestAssignees_auditAndDeletedItem(t *testing.T) {
	db, p := plainDB(t)
	it := mustItem(t, db, p.ID, "t")
	row, _, _ := db.AddAssignee(ctx, it.ID, "person:alice")

	rows := auditRows(t, db, "item_assignee", row.ID)
	if len(rows) != 1 || rows[0].Operation != "created" || !strings.Contains(rows[0].Detail.String, `"member":"person:alice"`) || !rows[0].Actor.Valid {
		t.Errorf("assignee audit: %+v", rows)
	}
	// The item's own history reads "assigned", as it always has.
	var last AuditEntry
	for _, r := range auditRows(t, db, "item", it.ID) {
		if r.Operation == "assigned" {
			last = r
		}
	}
	if !strings.Contains(last.Detail.String, `"to":"person:alice"`) {
		t.Errorf("item 'assigned' audit: %+v", last)
	}
	// A no-op writes nothing.
	n := len(auditRows(t, db, "item", it.ID))
	db.AddAssignee(ctx, it.ID, "person:alice")
	if len(auditRows(t, db, "item", it.ID)) != n {
		t.Error("a no-op add wrote audit")
	}
	db.SoftDelete(ctx, "item", it.ID)
	if _, _, err := db.AddAssignee(ctx, it.ID, "person:bob"); !errors.Is(err, ErrDeleted) {
		t.Errorf("add on a deleted item: %v", err)
	}
	if _, err := db.RemoveAssignee(ctx, it.ID, "person:alice"); !errors.Is(err, ErrDeleted) {
		t.Errorf("remove on a deleted item: %v", err)
	}
	if _, _, err := db.AddWatcher(ctx, it.ID, "person:bob"); !errors.Is(err, ErrDeleted) {
		t.Errorf("watch a deleted item: %v", err)
	}
}

func TestWatchers_itemAndProject(t *testing.T) {
	db, p := plainDB(t)
	it := mustItem(t, db, p.ID, "t")
	if _, added, err := db.AddWatcher(ctx, it.ID, "person:alice"); err != nil || !added {
		t.Fatal(err)
	}
	if _, added, _ := db.AddWatcher(ctx, it.ID, "person:alice"); added {
		t.Error("duplicate watcher added")
	}
	if got := watchersOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"person:alice"}) {
		t.Errorf("watchers = %v", got)
	}
	if removed, err := db.RemoveWatcher(ctx, it.ID, "person:alice"); err != nil || !removed {
		t.Errorf("remove: %v %v", removed, err)
	}
	// Watching does not make someone an assignee, and vice versa.
	db.AddWatcher(ctx, it.ID, "person:bob")
	if len(assigneesOf(t, db, it.ID)) != 0 {
		t.Error("a watcher became an assignee")
	}

	// Project watchers.
	if _, added, err := db.AddProjectWatcher(ctx, p.ID, "person:alice"); err != nil || !added {
		t.Fatal(err)
	}
	if _, added, _ := db.AddProjectWatcher(ctx, p.ID, "PERSON:alice"); added {
		t.Error("duplicate project watcher added")
	}
	ws, _ := db.ListProjectWatchers(ctx, p.ID)
	if len(ws) != 1 || ws[0].Member != "person:alice" {
		t.Errorf("project watchers: %+v", ws)
	}
	if _, _, err := db.AddProjectWatcher(ctx, p.ID, "program:x"); err == nil {
		t.Error("a program watched a project")
	}
	if removed, err := db.RemoveProjectWatcher(ctx, p.ID, "person:alice"); err != nil || !removed {
		t.Errorf("remove project watcher: %v %v", removed, err)
	}
	db.SoftDelete(ctx, "project", p.ID)
	if _, _, err := db.AddProjectWatcher(ctx, p.ID, "person:bob"); !errors.Is(err, ErrDeleted) {
		t.Errorf("watch a deleted project: %v", err)
	}
}

func TestAssignees_databaseEnforcesUniqueness(t *testing.T) {
	db, p := plainDB(t)
	it := mustItem(t, db, p.ID, "t")
	db.AddAssignee(ctx, it.ID, "person:alice")
	db.AddWatcher(ctx, it.ID, "person:alice")
	for _, q := range []string{
		`INSERT INTO item_assignees (id, item_id, member, created_at) VALUES ('a', '` + it.ID + `', 'person:alice', 'x')`,
		`INSERT INTO item_watchers (id, item_id, member, created_at) VALUES ('b', '` + it.ID + `', 'person:alice', 'x')`,
	} {
		if _, err := db.conn.Exec(q); err == nil {
			t.Errorf("a duplicate live row was inserted behind the tool layer: %s", q)
		}
	}
	// Soft-deleted duplicates do not count, and restore over a live one is refused.
	rows, _ := db.ListAssignees(ctx, it.ID)
	db.SoftDelete(ctx, "item_assignee", rows[0].ID)
	if _, added, err := db.AddAssignee(ctx, it.ID, "person:alice"); err != nil || !added {
		t.Fatalf("re-add after soft delete: %v %v", added, err)
	}
	if err := db.Restore(ctx, "item_assignee", rows[0].ID); err == nil {
		t.Error("restoring over a live duplicate should fail")
	}
	// Watchers are deletable entities too.
	ws, _ := db.ListWatchers(ctx, it.ID)
	if err := db.SoftDelete(ctx, "item_watcher", ws[0].ID); err != nil {
		t.Errorf("item_watcher must be deletable: %v", err)
	}
	pw, _, _ := db.AddProjectWatcher(ctx, p.ID, "person:alice")
	if err := db.SoftDelete(ctx, "project_watcher", pw.ID); err != nil {
		t.Errorf("project_watcher must be deletable: %v", err)
	}
}

func TestAssignees_listItemsFilters(t *testing.T) {
	db, p := plainDB(t)
	a := mustItem(t, db, p.ID, "a")
	b := mustItem(t, db, p.ID, "b")
	mustItem(t, db, p.ID, "c")
	db.AddAssignee(ctx, a.ID, "person:alice")
	db.AddAssignee(ctx, a.ID, "agent:claude-code")
	db.AddAssignee(ctx, b.ID, "agent:claude-code")
	db.AddWatcher(ctx, b.ID, "person:alice")

	got, _ := db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Assignee: "AGENT:claude-code"})
	if len(got) != 2 {
		t.Errorf("assignee filter: %d", len(got))
	}
	got, _ = db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Assignee: "person:alice"})
	if len(got) != 1 || got[0].ID != a.ID || !reflect.DeepEqual(got[0].Assignees, []string{"agent:claude-code", "person:alice"}) {
		t.Errorf("an item with two assignees matches by either, and lists both: %+v", got)
	}
	got, _ = db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Watcher: "person:alice"})
	if len(got) != 1 || got[0].ID != b.ID {
		t.Errorf("watcher filter: %+v", got)
	}
	db.RemoveAssignee(ctx, a.ID, "person:alice")
	if got, _ = db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Assignee: "person:alice"}); len(got) != 0 {
		t.Errorf("a removed assignee still matches: %d", len(got))
	}
}

func TestAssignees_updateItemFields(t *testing.T) {
	db, p := plainDB(t)
	it := mustItem(t, db, p.ID, "t")
	set := func(f ItemFields) (*Item, error) { return db.UpdateItemFields(ctx, it.ID, ItemUpdate{Fields: f}) }

	// Assignee keeps its meaning: replace the whole set with this one value.
	got, err := set(ItemFields{Assignee: ptr("person:alice")})
	if err != nil || !reflect.DeepEqual(got.Assignees, []string{"person:alice"}) {
		t.Fatalf("assignee: %v %v", got, err)
	}
	// The exact list.
	ls := []string{"person:bob", "agent:claude-code", "PERSON:bob"}
	got, err = set(ItemFields{Assignees: &ls})
	if err != nil || !reflect.DeepEqual(got.Assignees, []string{"agent:claude-code", "person:bob"}) {
		t.Fatalf("assignees: %v %v", got.Assignees, err)
	}
	// Unchanged writes nothing.
	n := len(auditRows(t, db, "item", it.ID))
	same := []string{"person:bob", "agent:claude-code"}
	if _, err := set(ItemFields{Assignees: &same}); err != nil {
		t.Fatal(err)
	}
	if len(auditRows(t, db, "item", it.ID)) != n {
		t.Error("an unchanged list wrote audit")
	}
	// Free text and old spellings are refused, and nothing changes.
	for _, bad := range []string{"Michael", "digital-michael", "program:x"} {
		if _, err := set(ItemFields{Assignee: ptr(bad)}); err == nil {
			t.Errorf("assignee %q accepted", bad)
		}
	}
	badList := []string{"person:ok", ""}
	if _, err := set(ItemFields{Assignees: &badList}); err == nil {
		t.Error("an empty entry accepted")
	}
	if got := assigneesOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"agent:claude-code", "person:bob"}) {
		t.Errorf("a refused edit changed the assignees: %v", got)
	}
	// Both forms together are refused; "" clears.
	one := "person:x"
	if _, err := set(ItemFields{Assignee: &one, Assignees: &same}); err == nil {
		t.Error("assignee with assignees accepted")
	}
	if got, err := set(ItemFields{Assignee: ptr("")}); err != nil || len(got.Assignees) != 0 || got.Assignee.Valid {
		t.Errorf("clear: %v %v", got, err)
	}
	// Watchers as an exact list.
	ws := []string{"person:alice", "person:bob"}
	if got, err := set(ItemFields{Watchers: &ws}); err != nil || !reflect.DeepEqual(got.Watchers, []string{"person:alice", "person:bob"}) {
		t.Errorf("watchers: %v %v", got, err)
	}
}

func TestAssignees_createDefaultsAndExplicit(t *testing.T) {
	dir := t.TempDir()

	// An agent creating a ticket in a project owned by a person: assigned to itself,
	// watched by the project's owners.
	adb := openAsActor(t, filepath.Join(dir, "a.db"), robot)
	ap := mustProject(t, adb, "demo")
	adb.AddProjectMember(ctx, ap.ID, "person:alice", "owner")
	adb.AddProjectMember(ctx, ap.ID, "person:bob", "owner")
	adb.AddProjectMember(ctx, ap.ID, "person:carol", "member") // a member is not an owner
	it := mustItem(t, adb, ap.ID, "by the agent")
	if !reflect.DeepEqual(it.Assignees, []string{"agent:claude-code"}) || !reflect.DeepEqual(it.Watchers, []string{"person:alice", "person:bob"}) {
		t.Errorf("agent default: assignees=%v watchers=%v", it.Assignees, it.Watchers)
	}
	// Explicit lists override; an empty list means none.
	got, err := adb.CreateItem(ctx, CreateItemParams{ProjectID: ap.ID, Title: "explicit", Assignees: []string{"person:carol"}, Watchers: []string{}})
	if err != nil || !reflect.DeepEqual(got.Assignees, []string{"person:carol"}) || len(got.Watchers) != 0 {
		t.Errorf("explicit: %v %v", got, err)
	}
	got, _ = adb.CreateItem(ctx, CreateItemParams{ProjectID: ap.ID, Title: "none", Assignees: []string{}})
	if len(got.Assignees) != 0 || !reflect.DeepEqual(got.Watchers, []string{"person:alice", "person:bob"}) {
		t.Errorf("an empty assignee list means unassigned, watchers still default: %v / %v", got.Assignees, got.Watchers)
	}
	// The compatibility single value.
	got, err = adb.CreateItem(ctx, CreateItemParams{ProjectID: ap.ID, Title: "one", Assignee: "person:bob"})
	if err != nil || !reflect.DeepEqual(got.Assignees, []string{"person:bob"}) {
		t.Errorf("single assignee: %v %v", got, err)
	}
	if _, err := adb.CreateItem(ctx, CreateItemParams{ProjectID: ap.ID, Title: "both", Assignee: "person:a", Assignees: []string{"person:b"}}); err == nil {
		t.Error("assignee with assignees accepted")
	}
	if _, err := adb.CreateItem(ctx, CreateItemParams{ProjectID: ap.ID, Title: "bad", Assignee: "Michael"}); err == nil {
		t.Error("free-text assignee accepted")
	}

	// A person, or a program, creating a ticket gets no default assignee: only an
	// agent is assigned to its own work.
	pdb := openAsActor(t, filepath.Join(dir, "p.db"), alice)
	pp := mustProject(t, pdb, "demo")
	if it := mustItem(t, pdb, pp.ID, "by a person"); len(it.Assignees) != 0 || len(it.Watchers) != 0 {
		t.Errorf("a person's ticket: %v %v", it.Assignees, it.Watchers)
	}
	gdb, gp := plainDB(t)
	if it := mustItem(t, gdb, gp.ID, "by a program"); len(it.Assignees) != 0 {
		t.Errorf("a program's ticket: %v", it.Assignees)
	}
	// Another agent is assigned to its own work, not to claude-code.
	other := openAsActor(t, filepath.Join(dir, "o.db"), Actor{Kind: ActorAgent, ID: "other-agent"})
	op := mustProject(t, other, "demo")
	if it := mustItem(t, other, op.ID, "by another agent"); !reflect.DeepEqual(it.Assignees, []string{"agent:other-agent"}) {
		t.Errorf("another agent: %v", it.Assignees)
	}
}

// ---- migration of the legacy items.assignee column ----

func withLegacyAssigneeColumn(t *testing.T, db *DB) {
	t.Helper()
	if err := ensureColumn(db.conn, "items", "assignee", "TEXT"); err != nil {
		t.Fatal(err)
	}
}

func TestAssignees_migrationCopiesLegacyValuesAndReportsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	db := openAs(t, path, script, nil)
	withLegacyAssigneeColumn(t, db)
	p := mustProject(t, db, "demo")
	mk := func(title, legacy string) string {
		it := mustItem(t, db, p.ID, title)
		if legacy != "" {
			db.conn.Exec(`UPDATE items SET assignee = ? WHERE id = ?`, legacy, it.ID)
		}
		return it.ID
	}
	a := mk("a", "digital-michael")
	b := mk("b", "Michael")
	c := mk("c", "person:alice") // already in the new form
	d := mk("d", "")
	e := mk("e", "a stranger") // not recognised: reported, never guessed
	db.Close()

	db = openAs(t, path, script, nil) // the copy runs
	for id, want := range map[string][]string{a: {"person:digital-michael"}, b: {"person:digital-michael"}, c: {"person:alice"}, d: {}, e: {}} {
		if got := assigneesOf(t, db, id); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", id[:6], got, want)
		}
	}
	rep, err := db.AssigneeMigrationReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || !reflect.DeepEqual(rep.Unresolved, []string{"a stranger"}) {
		t.Errorf("the report must flag the unrecognised value: %+v", rep)
	}
	if dropped, err := db.DropLegacyAssigneeColumn(ctx); err == nil || dropped || !strings.Contains(err.Error(), "a stranger") {
		t.Errorf("the drop must refuse while a value is unresolved: %v %v", dropped, err)
	}
	// Idempotent, and a removal is not resurrected.
	db.Close()
	db = openAs(t, path, script, nil)
	if _, err := db.RemoveAssignee(ctx, a, "person:digital-michael"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = openAs(t, path, script, nil)
	if got := assigneesOf(t, db, a); len(got) != 0 {
		t.Errorf("a removed assignee came back: %v", got)
	}
	// The operator decides what the stranger is; then the gate passes.
	db.AddAssignee(ctx, e, "person:stranger")
	rep, _ = db.AssigneeMigrationReport(ctx)
	if !rep.OK() {
		t.Errorf("after the value is resolved by hand the gate should pass: %+v", rep)
	}
}

func TestAssignees_dropAfterCopyKeepsEverythingWorking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	db := openAs(t, path, script, nil)
	withLegacyAssigneeColumn(t, db)
	p := mustProject(t, db, "demo")
	it := mustItem(t, db, p.ID, "t")
	db.conn.Exec(`UPDATE items SET assignee = 'digital-michael' WHERE id = ?`, it.ID)
	db.Close()

	db = openAs(t, path, script, nil)
	rep, _ := db.AssigneeMigrationReport(ctx)
	if !rep.OK() || rep.Legacy != 1 {
		t.Fatalf("gate: %+v", rep)
	}
	dropped, err := db.DropLegacyAssigneeColumn(ctx)
	if err != nil || !dropped {
		t.Fatalf("drop: %v %v", dropped, err)
	}
	if has, _ := hasColumn(db.conn, "items", "assignee"); has {
		t.Fatal("the column is still there")
	}
	if dropped, err := db.DropLegacyAssigneeColumn(ctx); err != nil || dropped {
		t.Errorf("second drop: %v %v", dropped, err)
	}
	if got := assigneesOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"person:digital-michael"}) {
		t.Errorf("the assignee was lost: %v", got)
	}
	// Everything still works without the column.
	n, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "after", Assignees: []string{"person:x", "agent:y"}})
	if err != nil || len(n.Assignees) != 2 {
		t.Fatalf("create: %v %v", n, err)
	}
	if _, err := db.UpdateItem(ctx, n.ID, UpdateItemParams{Title: ptr("renamed")}); err != nil {
		t.Errorf("update: %v", err)
	}
	if got, _ := db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Assignee: "person:x"}); len(got) != 1 {
		t.Errorf("filter: %d", len(got))
	}
	if _, err := db.MoveItem(ctx, n.ID, "demo", MoveOptions{}); err != nil && !strings.Contains(err.Error(), "already") {
		t.Errorf("move: %v", err)
	}
	if _, err := db.SummarizeProject(ctx, p.ID, "demo"); err != nil {
		t.Errorf("summary: %v", err)
	}
	if _, err := db.HealthFindings(ctx); err != nil {
		t.Errorf("health: %v", err)
	}
	// Reopening does not bring the column back.
	db.Close()
	db = openAs(t, path, script, nil)
	if has, _ := hasColumn(db.conn, "items", "assignee"); has {
		t.Error("reopening recreated items.assignee")
	}
	assertIntegrity(t, db)
}

func TestAssignees_aFreshDatabaseHasNoLegacyColumn(t *testing.T) {
	db, _ := plainDB(t)
	if has, _ := hasColumn(db.conn, "items", "assignee"); has {
		t.Error("a new database should not have items.assignee")
	}
}
