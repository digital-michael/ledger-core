package ledgercore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// created_by, owner and project_members (ledger story a94140e8). Every value is
// an actor "kind:id" string (identity.go).

var (
	alice  = Actor{Kind: ActorPerson, ID: "alice", Label: "Alice"}
	robot  = Actor{Kind: ActorAgent, ID: "claude-code", Label: "Claude Code"}
	script = Actor{Kind: ActorProgram, ID: "backfill", Label: "backfill"}
)

func openAsActor(t *testing.T, path string, a Actor) *DB {
	t.Helper()
	return openAs(t, path, a, nil)
}

func ownerOf(it *Item) string     { return it.Owner.String }
func createdByOf(it *Item) string { return it.CreatedBy.String }

// ---- items: created_by and owner ----

func TestPeople_createdByComesFromTheWriterAndNeverChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	db := openAsActor(t, path, robot)
	p := mustProject(t, db, "demo")
	it := mustItem(t, db, p.ID, "t")
	if createdByOf(it) != "agent:claude-code" {
		t.Errorf("created_by = %q", createdByOf(it))
	}
	// No edit path touches it: a retitle, a status change and an owner transfer leave it alone.
	db.UpdateItem(ctx, it.ID, UpdateItemParams{Title: ptr("renamed")})
	db.UpdateItemStatus(ctx, it.ID, "done")
	if _, err := db.TransferItemOwner(ctx, it.ID, "person:alice"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetItem(ctx, it.ID)
	if createdByOf(got) != "agent:claude-code" {
		t.Errorf("created_by moved to %q", createdByOf(got))
	}
	// A different writer reading it does not alter it either.
	other := openAsActor(t, path, alice)
	again, _ := other.GetItem(ctx, it.ID)
	if createdByOf(again) != "agent:claude-code" {
		t.Errorf("created_by changed on read: %q", createdByOf(again))
	}
}

func TestPeople_createdByIsEmptyWithoutAnActor(t *testing.T) {
	db := openAs(t, filepath.Join(t.TempDir(), "l.db"), Actor{}, nil)
	p := mustProject(t, db, "demo")
	it := mustItem(t, db, p.ID, "t")
	if it.CreatedBy.Valid {
		t.Errorf("an unknown writer must be stored as NULL, not a guess: %q", it.CreatedBy.String)
	}
}

func TestPeople_ownerDefaults(t *testing.T) {
	dir := t.TempDir()

	// A person who creates a ticket owns it.
	pdb := openAsActor(t, filepath.Join(dir, "a.db"), alice)
	pp := mustProject(t, pdb, "demo")
	if it := mustItem(t, pdb, pp.ID, "t"); ownerOf(it) != "person:alice" {
		t.Errorf("a person's ticket: owner = %q", ownerOf(it))
	}

	// An agent creating in a project nobody owns leaves the owner empty, not invented.
	path := filepath.Join(dir, "b.db")
	adb := openAsActor(t, path, robot)
	ap := mustProject(t, adb, "demo")
	it := mustItem(t, adb, ap.ID, "t")
	if it.Owner.Valid {
		t.Errorf("no person and no project owner: owner should be empty, got %q", it.Owner.String)
	}

	// Once the project has an owner, an agent's tickets default to it.
	if _, _, err := adb.AddProjectMember(ctx, ap.ID, "person:alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if got := mustItem(t, adb, ap.ID, "t2"); ownerOf(got) != "person:alice" {
		t.Errorf("agent ticket in an owned project: owner = %q", ownerOf(got))
	}

	// An explicit owner wins, and is checked.
	got, err := adb.CreateItem(ctx, CreateItemParams{ProjectID: ap.ID, Title: "t3", Owner: " Person:Bob "})
	if err != nil || ownerOf(got) != "person:bob" {
		t.Errorf("explicit owner: %v %v", got, err)
	}
	for _, bad := range []string{"agent:claude-code", "program:x", "Michael", "person:"} {
		if _, err := adb.CreateItem(ctx, CreateItemParams{ProjectID: ap.ID, Title: "bad", Owner: bad}); err == nil {
			t.Errorf("owner %q was accepted", bad)
		}
	}
}

func TestPeople_transferOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	db := openAsActor(t, path, alice)
	p := mustProject(t, db, "demo")
	it := mustItem(t, db, p.ID, "t")
	before, _ := db.GetItem(ctx, it.ID)

	got, err := db.TransferItemOwner(ctx, it.ID, " Person:Bob ")
	if err != nil || ownerOf(got) != "person:bob" {
		t.Fatalf("transfer: %v %v", got, err)
	}
	if got.UpdatedAt == before.UpdatedAt {
		t.Error("a transfer must move updated_at")
	}
	rows := auditRows(t, db, "item", it.ID)
	last := rows[len(rows)-1]
	if last.Operation != "owner_changed" || !strings.Contains(last.Detail.String, `"from":"person:alice"`) || !strings.Contains(last.Detail.String, `"to":"person:bob"`) {
		t.Errorf("audit: %+v", last)
	}
	if last.Actor.String != "person:alice" {
		t.Errorf("who transferred it: %q", last.Actor.String)
	}
	// The same owner again writes nothing.
	n := len(auditRows(t, db, "item", it.ID))
	if _, err := db.TransferItemOwner(ctx, it.ID, "person:bob"); err != nil {
		t.Fatal(err)
	}
	if len(auditRows(t, db, "item", it.ID)) != n {
		t.Error("a no-op transfer wrote audit")
	}
	// Only a person; never empty.
	for _, bad := range []string{"", "agent:claude-code", "program:x", "bob"} {
		if _, err := db.TransferItemOwner(ctx, it.ID, bad); err == nil {
			t.Errorf("transfer to %q accepted", bad)
		}
	}
	if got, _ := db.GetItem(ctx, it.ID); ownerOf(got) != "person:bob" {
		t.Errorf("a refused transfer changed the owner: %q", ownerOf(got))
	}
	// A deleted item is read-only; an unknown one is not found.
	db.SoftDelete(ctx, "item", it.ID)
	if _, err := db.TransferItemOwner(ctx, it.ID, "person:carol"); !errors.Is(err, ErrDeleted) {
		t.Errorf("transfer on a deleted item: %v", err)
	}
	if _, err := db.TransferItemOwner(ctx, "nope", "person:carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("transfer on an unknown item: %v", err)
	}
}

func TestPeople_transferGoesThroughThePolicy(t *testing.T) {
	var asked []Operation
	permit := func(_ context.Context, _ Actor, op Operation, _ Target) error {
		asked = append(asked, op)
		if op == OpTransferOwner {
			return errors.New("not allowed")
		}
		return nil
	}
	db := openAs(t, filepath.Join(t.TempDir(), "l.db"), alice, permit)
	p := mustProject(t, db, "demo")
	it := mustItem(t, db, p.ID, "t")
	if _, err := db.TransferItemOwner(ctx, it.ID, "person:bob"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("the policy was not consulted: %v", err)
	}
	if got, _ := db.GetItem(ctx, it.ID); ownerOf(got) != "person:alice" {
		t.Errorf("a refused transfer changed the owner")
	}
}

func TestPeople_listItemsFiltersByOwnerAndCreator(t *testing.T) {
	db := openAsActor(t, filepath.Join(t.TempDir(), "l.db"), alice)
	p := mustProject(t, db, "demo")
	a := mustItem(t, db, p.ID, "a")
	b := mustItem(t, db, p.ID, "b")
	db.TransferItemOwner(ctx, b.ID, "person:bob")
	got, _ := db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Owner: "PERSON:bob"})
	if len(got) != 1 || got[0].ID != b.ID {
		t.Errorf("owner filter: %v", got)
	}
	got, _ = db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Owner: "person:alice"})
	if len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("owner filter alice: %v", got)
	}
	got, _ = db.ListItems(ctx, ItemFilter{ProjectID: p.ID, CreatedBy: "person:alice"})
	if len(got) != 2 {
		t.Errorf("created_by filter: %d", len(got))
	}
}

// ---- projects and members ----

func TestPeople_projectCreatedByAndFirstOwner(t *testing.T) {
	dir := t.TempDir()
	pdb := openAsActor(t, filepath.Join(dir, "a.db"), alice)
	pp := mustProject(t, pdb, "demo")
	if by, _ := pdb.ProjectCreatedBy(ctx, pp.ID); by != "person:alice" {
		t.Errorf("project created_by = %q", by)
	}
	ms, _ := pdb.ListProjectMembers(ctx, pp.ID)
	if len(ms) != 1 || ms[0].Member != "person:alice" || ms[0].Role != "owner" {
		t.Errorf("the creating person should be the first owner: %+v", ms)
	}

	// An agent creates a project: recorded as creator, but it cannot be an owner.
	adb := openAsActor(t, filepath.Join(dir, "b.db"), robot)
	ap := mustProject(t, adb, "demo")
	if by, _ := adb.ProjectCreatedBy(ctx, ap.ID); by != "agent:claude-code" {
		t.Errorf("agent project created_by = %q", by)
	}
	if ms, _ := adb.ListProjectMembers(ctx, ap.ID); len(ms) != 0 {
		t.Errorf("an agent must not become an owner by creating a project: %+v", ms)
	}
}

func TestPeople_memberAddListAndRules(t *testing.T) {
	db := openAsActor(t, filepath.Join(t.TempDir(), "l.db"), alice)
	p := mustProject(t, db, "demo")

	m, added, err := db.AddProjectMember(ctx, p.ID, " Person:Bob ", "member")
	if err != nil || !added || m.Member != "person:bob" || m.Role != "member" {
		t.Fatalf("add: %+v %v %v", m, added, err)
	}
	again, added, err := db.AddProjectMember(ctx, p.ID, "person:bob", "member")
	if err != nil || added || again.ID != m.ID {
		t.Errorf("same member and role again should be a no-op: %v %v", added, err)
	}
	if _, _, err := db.AddProjectMember(ctx, p.ID, "person:bob", "viewer"); err == nil || !strings.Contains(err.Error(), "SetProjectMemberRole") && !strings.Contains(err.Error(), "role") {
		t.Errorf("a different role for an existing member should point at the role change: %v", err)
	}
	// An agent may be a member or viewer, never an owner.
	if _, _, err := db.AddProjectMember(ctx, p.ID, "agent:claude-code", "member"); err != nil {
		t.Errorf("agent member: %v", err)
	}
	if _, _, err := db.AddProjectMember(ctx, p.ID, "agent:other", "owner"); err == nil {
		t.Error("an agent owner was accepted")
	}
	for _, bad := range []struct{ who, role string }{{"program:x", "member"}, {"Bob", "member"}, {"person:carol", "admin"}, {"person:carol", ""}} {
		if _, _, err := db.AddProjectMember(ctx, p.ID, bad.who, bad.role); err == nil {
			t.Errorf("add %q as %q accepted", bad.who, bad.role)
		}
	}
	list, _ := db.ListProjectMembers(ctx, p.ID)
	var got []string
	for _, x := range list {
		got = append(got, x.Role+":"+x.Member)
	}
	if !reflect.DeepEqual(got, []string{"owner:person:alice", "member:agent:claude-code", "member:person:bob"}) {
		t.Errorf("list order (owners first, then by name): %v", got)
	}
	// Audited, with the actor.
	rows := auditRows(t, db, "project_member", m.ID)
	if len(rows) != 1 || rows[0].Operation != "created" || !strings.Contains(rows[0].Detail.String, `"role":"member"`) {
		t.Errorf("audit: %+v", rows)
	}
	// A deleted project takes no new members.
	db.SoftDelete(ctx, "project", p.ID)
	if _, _, err := db.AddProjectMember(ctx, p.ID, "person:dave", "member"); !errors.Is(err, ErrDeleted) {
		t.Errorf("member added to a deleted project: %v", err)
	}
}

func TestPeople_theLastOwnerCannotBeRemovedOrDemoted(t *testing.T) {
	db := openAsActor(t, filepath.Join(t.TempDir(), "l.db"), alice)
	p := mustProject(t, db, "demo")

	if _, err := db.RemoveProjectMember(ctx, p.ID, "person:alice"); !errors.Is(err, ErrLastOwner) {
		t.Errorf("removing the last owner: %v", err)
	}
	if _, err := db.SetProjectMemberRole(ctx, p.ID, "person:alice", "viewer"); !errors.Is(err, ErrLastOwner) {
		t.Errorf("demoting the last owner: %v", err)
	}
	// The generic soft delete must not be a way round it.
	ms, _ := db.ListProjectMembers(ctx, p.ID)
	if err := db.SoftDelete(ctx, "project_member", ms[0].ID); !errors.Is(err, ErrLastOwner) {
		t.Errorf("soft-deleting the last owner row: %v", err)
	}
	if ms, _ := db.ListProjectMembers(ctx, p.ID); len(ms) != 1 || ms[0].Role != "owner" {
		t.Fatalf("the owner is gone: %+v", ms)
	}

	// With a second owner, the first can go.
	if _, _, err := db.AddProjectMember(ctx, p.ID, "person:bob", "owner"); err != nil {
		t.Fatal(err)
	}
	if removed, err := db.RemoveProjectMember(ctx, p.ID, "person:alice"); err != nil || !removed {
		t.Fatalf("removing one of two owners: %v %v", removed, err)
	}
	if _, err := db.SetProjectMemberRole(ctx, p.ID, "person:bob", "member"); !errors.Is(err, ErrLastOwner) {
		t.Errorf("bob is now the last owner: %v", err)
	}
	// Removing a non-member reports false.
	if removed, err := db.RemoveProjectMember(ctx, p.ID, "person:nobody"); err != nil || removed {
		t.Errorf("removing a non-member: %v %v", removed, err)
	}
}

func TestPeople_setRole(t *testing.T) {
	db := openAsActor(t, filepath.Join(t.TempDir(), "l.db"), alice)
	p := mustProject(t, db, "demo")
	db.AddProjectMember(ctx, p.ID, "person:bob", "viewer")
	m, err := db.SetProjectMemberRole(ctx, p.ID, "person:bob", "member")
	if err != nil || m.Role != "member" {
		t.Fatalf("set role: %+v %v", m, err)
	}
	rows := auditRows(t, db, "project_member", m.ID)
	last := rows[len(rows)-1]
	if last.Operation != "updated" || !strings.Contains(last.Detail.String, `"from":"viewer"`) || !strings.Contains(last.Detail.String, `"to":"member"`) {
		t.Errorf("audit: %+v", last)
	}
	// Same role: nothing written.
	n := len(auditRows(t, db, "project_member", m.ID))
	db.SetProjectMemberRole(ctx, p.ID, "person:bob", "member")
	if len(auditRows(t, db, "project_member", m.ID)) != n {
		t.Error("a no-op role change wrote audit")
	}
	// Promote to owner (a person), but not an agent.
	if _, err := db.SetProjectMemberRole(ctx, p.ID, "person:bob", "owner"); err != nil {
		t.Errorf("promote: %v", err)
	}
	db.AddProjectMember(ctx, p.ID, "agent:claude-code", "member")
	if _, err := db.SetProjectMemberRole(ctx, p.ID, "agent:claude-code", "owner"); err == nil {
		t.Error("an agent was promoted to owner")
	}
	if _, err := db.SetProjectMemberRole(ctx, p.ID, "person:ghost", "member"); !errors.Is(err, ErrNotFound) {
		t.Errorf("set role on a non-member: %v", err)
	}
	if _, err := db.SetProjectMemberRole(ctx, p.ID, "person:bob", "boss"); err == nil {
		t.Error("an unknown role was accepted")
	}
}

func TestPeople_membershipIsEnforcedByTheDatabaseToo(t *testing.T) {
	db := openAsActor(t, filepath.Join(t.TempDir(), "l.db"), alice)
	p := mustProject(t, db, "demo")
	db.AddProjectMember(ctx, p.ID, "person:bob", "member")
	// Behind the package: a duplicate live member is refused by the index...
	if _, err := db.conn.Exec(`INSERT INTO project_members (id, project_id, member, role, created_at) VALUES ('x', ?, 'person:bob', 'viewer', '2026-01-01T00:00:00Z')`, p.ID); err == nil {
		t.Error("a duplicate live member was inserted behind the tool layer")
	}
	// ...and an unknown role by the vocabulary trigger.
	if _, err := db.conn.Exec(`INSERT INTO project_members (id, project_id, member, role, created_at) VALUES ('y', ?, 'person:carol', 'boss', '2026-01-01T00:00:00Z')`, p.ID); err == nil {
		t.Error("an unknown role was inserted behind the tool layer")
	}
	// A removed member can be added again, and a restore over a live duplicate is refused.
	db.RemoveProjectMember(ctx, p.ID, "person:bob")
	m, added, err := db.AddProjectMember(ctx, p.ID, "person:bob", "member")
	if err != nil || !added {
		t.Fatalf("re-add: %v %v", added, err)
	}
	var oldID string
	db.conn.QueryRow(`SELECT id FROM project_members WHERE member = 'person:bob' AND deleted_at IS NOT NULL`).Scan(&oldID)
	if oldID == "" || oldID == m.ID {
		t.Fatalf("expected a removed row: %q", oldID)
	}
	if err := db.Restore(ctx, "project_member", oldID); err == nil {
		t.Error("restoring over a live duplicate should fail")
	}
}

func TestPeople_vocabularyIncludesMemberRoles(t *testing.T) {
	if got := Vocab().MemberRoles; !reflect.DeepEqual(got, []string{"owner", "member", "viewer"}) {
		t.Errorf("member roles = %v", got)
	}
}

// ---- the backfill ----

func TestPeople_creatorFromAuditMapping(t *testing.T) {
	cases := []struct{ actor, client, want, source string }{
		{"program:claude-code", "claude-code", "agent:claude-code", "audit actor"},
		{"program:mcp", "mcp", "program:mcp", "audit actor"},
		{"program:ledger-server", "the ledger web UI", "program:ledger-server", "audit actor"},
		{"person:alice", "x", "person:alice", "audit actor"},
		{"", "claude-code", "agent:claude-code", "audit client"},
		{"", "mcp-console/script_file", "program:mcp-console", "audit client"},
		{"", "mcp-console/run", "program:mcp-console", "audit client"},
		{"", "the ledger web UI", "program:ledger-server", "audit client"},
		{"", "mcp", "program:mcp", "audit client"},
		{"", "", "person:digital-michael", "no record"},
		{"", "something-unknown", "person:digital-michael", "no record"},
	}
	for _, c := range cases {
		got, src := CreatorFromAudit(c.actor, c.client, "person:digital-michael")
		if got != c.want || src != c.source {
			t.Errorf("(%q, %q) = %q via %q; want %q via %q", c.actor, c.client, got, src, c.want, c.source)
		}
	}
}

// legacyShape rewinds a ledger to how it looked before these fields existed:
// no creator, no owner, no members, and audit rows the way old writers left them.
func legacyShape(t *testing.T, db *DB) {
	t.Helper()
	for _, q := range []string{
		`UPDATE items SET created_by = NULL, owner = NULL`,
		`UPDATE projects SET created_by = NULL`,
		`DELETE FROM project_members`,
	} {
		if _, err := db.conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPeople_backfillMapsFromTheAuditTrailAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	db := openAsActor(t, path, robot)
	p := mustProject(t, db, "demo")
	q := mustProject(t, db, "second")
	viaAgent := mustItem(t, db, p.ID, "by the agent")
	viaClient := mustItem(t, db, p.ID, "by client only")
	viaConsole := mustItem(t, db, q.ID, "by mcp-console")
	noRecord := mustItem(t, db, q.ID, "no record")
	db.SoftDelete(ctx, "item", noRecord.ID) // deleted items are backfilled too

	// Old writers left audit rows with no actor, only a client name.
	set := func(id, actor, client string) {
		if _, err := db.conn.Exec(`UPDATE audit_log SET actor = NULLIF(?, ''), client = NULLIF(?, '') WHERE entity_type = 'item' AND entity_id = ? AND operation = 'created'`, actor, client, id); err != nil {
			t.Fatal(err)
		}
	}
	set(viaClient.ID, "", "claude-code")
	set(viaConsole.ID, "", "mcp-console/script_file")
	set(noRecord.ID, "", "")
	legacyShape(t, db)

	// Dry run: reports, changes nothing.
	rep, err := db.BackfillPeople(ctx, BackfillOptions{Owner: "person:alice", Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items != 4 || rep.Projects != 2 || rep.OwnersAdded != 2 || rep.Applied {
		t.Errorf("dry run report: %+v", rep)
	}
	if it, _ := db.GetItem(ctx, viaAgent.ID); it.CreatedBy.Valid || it.Owner.Valid {
		t.Error("a dry run changed data")
	}

	rep, err = db.BackfillPeople(ctx, BackfillOptions{Owner: "person:alice", Apply: true})
	if err != nil || !rep.Applied {
		t.Fatalf("apply: %+v %v", rep, err)
	}
	want := map[string]string{
		viaAgent.ID:   "agent:claude-code",
		viaClient.ID:  "agent:claude-code",
		viaConsole.ID: "program:mcp-console",
		noRecord.ID:   "person:alice",
	}
	for id, by := range want {
		it, _ := db.GetItem(ctx, id)
		if createdByOf(it) != by || ownerOf(it) != "person:alice" {
			t.Errorf("%s: created_by=%q owner=%q, want %q / person:alice", it.Title, createdByOf(it), ownerOf(it), by)
		}
	}
	for _, id := range []string{p.ID, q.ID} {
		ms, _ := db.ListProjectMembers(ctx, id)
		if len(ms) != 1 || ms[0].Member != "person:alice" || ms[0].Role != "owner" {
			t.Errorf("project %s members: %+v", id[:6], ms)
		}
		if by, _ := db.ProjectCreatedBy(ctx, id); by != "agent:claude-code" {
			t.Errorf("project created_by = %q", by)
		}
	}
	// One batch ties the audit rows together.
	rows, _ := db.ListAuditLog(ctx, AuditFilter{EntityType: "item", EntityID: viaAgent.ID})
	last := rows[len(rows)-1]
	if last.Operation != "updated" || !last.Batch.Valid || !strings.Contains(last.Detail.String, "created_by") {
		t.Errorf("backfill audit row: %+v", last)
	}

	// Idempotent: a second run changes nothing, and writes no new audit rows.
	before, _ := db.ListAuditLog(ctx, AuditFilter{})
	rep, err = db.BackfillPeople(ctx, BackfillOptions{Owner: "person:alice", Apply: true})
	if err != nil || rep.Items != 0 || rep.Projects != 0 || rep.OwnersAdded != 0 {
		t.Errorf("second run: %+v %v", rep, err)
	}
	if after, _ := db.ListAuditLog(ctx, AuditFilter{}); len(after) != len(before) {
		t.Errorf("the second run wrote audit rows: %d -> %d", len(before), len(after))
	}

	// It never overwrites a value that is already set.
	db.conn.Exec(`UPDATE items SET owner = 'person:bob' WHERE id = ?`, viaAgent.ID)
	db.BackfillPeople(ctx, BackfillOptions{Owner: "person:alice", Apply: true})
	if it, _ := db.GetItem(ctx, viaAgent.ID); ownerOf(it) != "person:bob" {
		t.Errorf("backfill overwrote an existing owner: %q", ownerOf(it))
	}
}

func TestPeople_backfillDoesNotAddAnOwnerToAProjectThatHasOne(t *testing.T) {
	db := openAsActor(t, filepath.Join(t.TempDir(), "l.db"), alice)
	p := mustProject(t, db, "demo")
	db.AddProjectMember(ctx, p.ID, "person:bob", "owner")
	rep, err := db.BackfillPeople(ctx, BackfillOptions{Owner: "person:carol", Apply: true})
	if err != nil || rep.OwnersAdded != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	ms, _ := db.ListProjectMembers(ctx, p.ID)
	if len(ms) != 2 {
		t.Errorf("members: %+v", ms)
	}
}

func TestPeople_backfillRefusesABadOwner(t *testing.T) {
	db := openAsActor(t, filepath.Join(t.TempDir(), "l.db"), alice)
	for _, bad := range []string{"", "agent:claude-code", "Michael"} {
		if _, err := db.BackfillPeople(ctx, BackfillOptions{Owner: bad, Apply: true}); err == nil {
			t.Errorf("owner %q accepted", bad)
		}
	}
}

// Older databases get the new columns and table on open, with nothing lost.
func TestPeople_migrationAddsColumnsToAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	db := openAsActor(t, path, alice)
	p := mustProject(t, db, "demo")
	it := mustItem(t, db, p.ID, "kept")
	for _, q := range []string{
		`DROP TABLE project_members`,
		`DROP INDEX idx_items_owner`,
		`ALTER TABLE items DROP COLUMN created_by`,
		`ALTER TABLE items DROP COLUMN owner`,
		`ALTER TABLE projects DROP COLUMN created_by`,
	} {
		if _, err := db.conn.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	db = openAsActor(t, path, alice)
	got, err := db.GetItem(ctx, it.ID)
	if err != nil || got.Title != "kept" || got.CreatedBy.Valid {
		t.Fatalf("after the upgrade: %+v %v", got, err)
	}
	if _, _, err := db.AddProjectMember(ctx, p.ID, "person:bob", "member"); err != nil {
		t.Errorf("members after the upgrade: %v", err)
	}
	db.Close()
	db = openAsActor(t, path, alice) // and again: idempotent
	if ms, _ := db.ListProjectMembers(ctx, p.ID); len(ms) != 1 {
		t.Errorf("members after a second open: %+v", ms)
	}
}
