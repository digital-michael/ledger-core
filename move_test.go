package ledgercore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func mustChild(t *testing.T, db *DB, projectID, parentID, title string) *Item {
	t.Helper()
	it, err := db.CreateItem(ctx, CreateItemParams{ProjectID: projectID, ParentID: parentID, Title: title})
	if err != nil {
		t.Fatalf("CreateItem %q: %v", title, err)
	}
	return it
}

func itemProject(t *testing.T, db *DB, id string) string {
	t.Helper()
	it, err := db.GetItem(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return it.ProjectID
}

func countWhere(t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.conn.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMoveOneItemKeepsEverythingAboutIt(t *testing.T) {
	db := openTest(t)
	a, b, c := mustProject(t, db, "a"), mustProject(t, db, "b"), mustProject(t, db, "c")
	it := mustItem(t, db, a.ID, "the ticket")
	other := mustItem(t, db, a.ID, "stays")
	elsewhere := mustItem(t, db, c.ID, "in a third project")
	if _, err := db.AddNote(ctx, AddNoteParams{ProjectID: a.ID, ItemID: it.ID, Body: "a note"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddResource(ctx, a.ID, it.ID, "https://example.test/x", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RelateItems(ctx, it.ID, other.ID, "related_to"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RelateItems(ctx, elsewhere.ID, it.ID, "related_to"); err != nil {
		t.Fatal(err)
	}

	res, err := db.MoveItem(ctx, it.ID, "b", MoveOptions{})
	if err != nil {
		t.Fatalf("MoveItem: %v", err)
	}
	if res.FromProject != "a" || res.ToProject != "b" || len(res.Items) != 1 || res.Items[0].ID != it.ID {
		t.Fatalf("result: %+v", res)
	}
	if res.Notes != 1 || res.Resources != 1 {
		t.Errorf("notes=%d resources=%d, want 1 and 1", res.Notes, res.Resources)
	}
	// "other" stays in a, "elsewhere" is in c: both are outside the target, so both relations are reported.
	if res.Relations != 2 {
		t.Errorf("relations reported = %d, want 2", res.Relations)
	}

	got, err := db.GetItem(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectID != b.ID || got.Title != "the ticket" {
		t.Errorf("after the move: project=%s title=%q", got.ProjectID, got.Title)
	}
	if got.UpdatedAt == it.UpdatedAt {
		t.Errorf("updated_at was not bumped")
	}
	if n := countWhere(t, db, `SELECT COUNT(*) FROM notes WHERE item_id = ? AND project_id = ?`, it.ID, b.ID); n != 1 {
		t.Errorf("notes in the new project = %d", n)
	}
	if n := countWhere(t, db, `SELECT COUNT(*) FROM resources WHERE item_id = ? AND project_id = ?`, it.ID, b.ID); n != 1 {
		t.Errorf("resources in the new project = %d", n)
	}
	if n := countWhere(t, db, `SELECT COUNT(*) FROM item_relations WHERE (from_item_id = ? OR to_item_id = ?) AND deleted_at IS NULL`, it.ID, it.ID); n != 2 {
		t.Errorf("relations kept = %d, want 2", n)
	}
	if itemProject(t, db, other.ID) != a.ID {
		t.Errorf("an item that was not moved changed project")
	}

	rows := auditRows(t, db, "item", it.ID)
	last := rows[len(rows)-1]
	if last.Operation != "moved" || !strings.Contains(last.Detail.String, `"from_project":"a"`) || !strings.Contains(last.Detail.String, `"to_project":"b"`) {
		t.Errorf("audit row: %+v", last)
	}
	if last.Client.String != testClient {
		t.Errorf("audit client = %q", last.Client.String)
	}
}

func TestMoveSubtreeTogetherIncludingDeletedChildren(t *testing.T) {
	db := openTest(t)
	a, b := mustProject(t, db, "a"), mustProject(t, db, "b")
	root := mustItem(t, db, a.ID, "story")
	k1 := mustChild(t, db, a.ID, root.ID, "child 1")
	k2 := mustChild(t, db, a.ID, root.ID, "child 2")
	g := mustChild(t, db, a.ID, k1.ID, "grandchild")
	gone := mustChild(t, db, a.ID, root.ID, "deleted child")
	if err := db.SoftDelete(ctx, "item", gone.ID); err != nil {
		t.Fatal(err)
	}
	unrelated := mustItem(t, db, a.ID, "unrelated")

	res, err := db.MoveItem(ctx, root.ID, "b", MoveOptions{WithChildren: true})
	if err != nil {
		t.Fatalf("MoveItem: %v", err)
	}
	if len(res.Items) != 5 || res.Items[0].ID != root.ID {
		t.Fatalf("moved %d items, first %q; want 5 with the root first", len(res.Items), res.Items[0].ID)
	}
	for _, id := range []string{root.ID, k1.ID, k2.ID, g.ID, gone.ID} {
		if itemProject(t, db, id) != b.ID {
			t.Errorf("%s did not move", id)
		}
	}
	if itemProject(t, db, unrelated.ID) != a.ID {
		t.Errorf("an unrelated item moved")
	}
	// The tree is intact, and the deleted child is still deleted.
	if kid, _ := db.GetItem(ctx, g.ID); kid.ParentID.String != k1.ID {
		t.Errorf("grandchild's parent = %q", kid.ParentID.String)
	}
	if d, _ := db.GetItem(ctx, gone.ID); !d.DeletedAt.Valid {
		t.Errorf("the deleted child came back to life")
	}

	// One act: every audit row shares a batch id.
	batch := ""
	for _, id := range []string{root.ID, k1.ID, k2.ID, g.ID, gone.ID} {
		rows := auditRows(t, db, "item", id)
		last := rows[len(rows)-1]
		if last.Operation != "moved" || !last.Batch.Valid {
			t.Fatalf("%s: audit %+v", id, last)
		}
		if batch == "" {
			batch = last.Batch.String
		} else if batch != last.Batch.String {
			t.Errorf("audit rows do not share a batch")
		}
	}
}

func TestMoveRefusals(t *testing.T) {
	db := openTest(t)
	a, b := mustProject(t, db, "a"), mustProject(t, db, "b")
	parent := mustItem(t, db, a.ID, "parent")
	kid := mustChild(t, db, a.ID, parent.ID, "kid")

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"children without with_children", func() error { _, e := db.MoveItem(ctx, parent.ID, "b", MoveOptions{}); return e }, "descendants"},
		{"child whose parent stays", func() error { _, e := db.MoveItem(ctx, kid.ID, "b", MoveOptions{}); return e }, "parent link across projects"},
		{"unknown item", func() error { _, e := db.MoveItem(ctx, "nope", "b", MoveOptions{}); return e }, "not found"},
		{"unknown project", func() error { _, e := db.MoveItem(ctx, parent.ID, "zzz", MoveOptions{WithChildren: true}); return e }, "never creates a project"},
		{"same project", func() error { _, e := db.MoveItem(ctx, parent.ID, "a", MoveOptions{WithChildren: true}); return e }, "already in project"},
	}
	for _, c := range cases {
		err := c.run()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
	projects, _ := db.ListProjects(ctx)
	if len(projects) != 2 {
		t.Errorf("a refused move created a project: %d projects", len(projects))
	}
	if itemProject(t, db, parent.ID) != a.ID || itemProject(t, db, kid.ID) != a.ID {
		t.Errorf("a refused move changed something")
	}

	// A soft-deleted item is read-only until restored.
	lone := mustItem(t, db, a.ID, "deleted")
	if err := db.SoftDelete(ctx, "item", lone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MoveItem(ctx, lone.ID, "b", MoveOptions{}); !errors.Is(err, ErrDeleted) {
		t.Errorf("moving a deleted item: %v", err)
	}
	// A soft-deleted target is surfaced, not revived.
	if err := db.SoftDelete(ctx, "project", b.ID); err != nil {
		t.Fatal(err)
	}
	var sd *ErrProjectSoftDeleted
	live := mustItem(t, db, a.ID, "live")
	if _, err := db.MoveItem(ctx, live.ID, "b", MoveOptions{}); !errors.As(err, &sd) {
		t.Errorf("moving into a deleted project: %v", err)
	}
}

func TestMoveOutFromUnderAParentNeedsDetach(t *testing.T) {
	db := openTest(t)
	a, b := mustProject(t, db, "a"), mustProject(t, db, "b")
	parent := mustItem(t, db, a.ID, "parent")
	kid := mustChild(t, db, a.ID, parent.ID, "kid")

	res, err := db.MoveItem(ctx, kid.ID, "b", MoveOptions{DetachParent: true})
	if err != nil {
		t.Fatalf("MoveItem: %v", err)
	}
	if !res.Detached {
		t.Errorf("result did not say the item was detached")
	}
	got, _ := db.GetItem(ctx, kid.ID)
	if got.ProjectID != b.ID || got.ParentID.Valid {
		t.Errorf("project=%s parent=%q", got.ProjectID, got.ParentID.String)
	}
	rows := auditRows(t, db, "item", kid.ID)
	if d := rows[len(rows)-1].Detail.String; !strings.Contains(d, parent.ID) {
		t.Errorf("the audit row does not record the old parent: %s", d)
	}
}

func TestMoveDryRunChangesNothing(t *testing.T) {
	db := openTest(t)
	a, _ := mustProject(t, db, "a"), mustProject(t, db, "b")
	root := mustItem(t, db, a.ID, "story")
	mustChild(t, db, a.ID, root.ID, "kid")
	before := countWhere(t, db, `SELECT COUNT(*) FROM audit_log`)

	res, err := db.MoveItem(ctx, root.ID, "b", MoveOptions{WithChildren: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Items) != 2 {
		t.Fatalf("result: %+v", res)
	}
	if itemProject(t, db, root.ID) != a.ID {
		t.Errorf("a dry run moved the item")
	}
	if after := countWhere(t, db, `SELECT COUNT(*) FROM audit_log`); after != before {
		t.Errorf("a dry run wrote %d audit rows", after-before)
	}
}

func TestMoveComponentTitleMustBeUniqueInTheTarget(t *testing.T) {
	db := openTest(t)
	a, b := mustProject(t, db, "a"), mustProject(t, db, "b")
	for _, p := range []*Project{a, b} {
		if _, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Type: "component", Title: "auth"}); err != nil {
			t.Fatal(err)
		}
	}
	comp, _ := db.ListItems(ctx, ItemFilter{ProjectID: a.ID, Type: "component"})
	if _, err := db.MoveItem(ctx, comp[0].ID, "b", MoveOptions{}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want a duplicate-component refusal", err)
	}
	if itemProject(t, db, comp[0].ID) != a.ID {
		t.Errorf("the refused move changed the component")
	}
}

func TestMoveIsAskedOfThePolicy(t *testing.T) {
	var asked []Operation
	deny := func(_ context.Context, _ Actor, op Operation, _ Target) error {
		asked = append(asked, op)
		if op == OpMoveItem {
			return errors.New("not allowed")
		}
		return nil
	}
	db := openAs(t, filepath.Join(t.TempDir(), "l.db"), Actor{Kind: ActorProgram, ID: "p", Label: "p"}, deny)
	a, _ := mustProject(t, db, "a"), mustProject(t, db, "b")
	it := mustItem(t, db, a.ID, "t")
	if _, err := db.MoveItem(ctx, it.ID, "b", MoveOptions{}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("err = %v", err)
	}
	if itemProject(t, db, it.ID) != a.ID {
		t.Errorf("a refused move changed the item")
	}
	found := false
	for _, op := range asked {
		found = found || op == OpMoveItem
	}
	if !found {
		t.Errorf("the policy was never asked about OpMoveItem: %v", asked)
	}
}

func TestRenameProjectChangesTheNameNotTheKey(t *testing.T) {
	db := openTest(t)
	key := "/Users/me/Photon Datum/space_sim"
	p, err := db.GetOrCreateProjectForWrite(ctx, key, "space_sim")
	if err != nil {
		t.Fatal(err)
	}
	it := mustItem(t, db, p.ID, "a ticket")

	got, err := db.RenameProject(ctx, key, "  Space Sim  ")
	if err != nil {
		t.Fatalf("RenameProject: %v", err)
	}
	if got.Name != "Space Sim" || got.Key != key || got.ID != p.ID {
		t.Errorf("after the rename: %+v", got)
	}
	again, _ := db.GetOrCreateProject(ctx, key, "ignored")
	if again.Name != "Space Sim" {
		t.Errorf("the name did not persist: %q", again.Name)
	}
	if itemProject(t, db, it.ID) != p.ID {
		t.Errorf("an item lost its project")
	}
	rows := auditRows(t, db, "project", p.ID)
	last := rows[len(rows)-1]
	if last.Operation != "renamed" || !strings.Contains(last.Detail.String, `"from":"space_sim"`) || !strings.Contains(last.Detail.String, `"to":"Space Sim"`) {
		t.Errorf("audit row: %+v", last)
	}

	// The same name again writes nothing.
	before := len(auditRows(t, db, "project", p.ID))
	if _, err := db.RenameProject(ctx, key, "Space Sim"); err != nil {
		t.Fatal(err)
	}
	if after := len(auditRows(t, db, "project", p.ID)); after != before {
		t.Errorf("an unchanged rename wrote %d audit rows", after-before)
	}
}

func TestRenameProjectRefusals(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "p")
	for name, want := range map[string]string{
		"":                       "cannot be empty",
		"   ":                    "cannot be empty",
		"two\nlines":             "control character",
		strings.Repeat("x", 201): "at most",
	} {
		if _, err := db.RenameProject(ctx, "p", name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("name %q: err = %v, want %q", name, err, want)
		}
	}
	if _, err := db.RenameProject(ctx, "nope", "X"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	if err := db.SoftDelete(ctx, "project", p.ID); err != nil {
		t.Fatal(err)
	}
	var sd *ErrProjectSoftDeleted
	if _, err := db.RenameProject(ctx, "p", "X"); !errors.As(err, &sd) {
		t.Errorf("deleted project: %v", err)
	}
	// Spaces and punctuation are fine.
	q := mustProject(t, db, "q")
	if got, err := db.RenameProject(ctx, "q", "Space Sim: the (second) – ünï"); err != nil || got.ID != q.ID {
		t.Errorf("a name with spaces and punctuation: %v", err)
	}
}
