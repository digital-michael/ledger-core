package ledgercore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenameProjectKeyKeepsTheOldKeyWorking(t *testing.T) {
	db := openTest(t)
	old := "/Users/me/Photon Datum/space_sim"
	p := mustProject(t, db, old)
	it := mustItem(t, db, p.ID, "a ticket")

	got, err := db.RenameProjectKey(ctx, old, "space-sim")
	if err != nil {
		t.Fatalf("RenameProjectKey: %v", err)
	}
	if got.ID != p.ID || got.Key != "space-sim" {
		t.Fatalf("after the rename: %+v", got)
	}

	// The old key still reaches the same project, for reads and for writes: no new project appears.
	for _, name := range []string{"read", "write"} {
		var via *Project
		if name == "read" {
			via, err = db.GetOrCreateProject(ctx, old, old)
		} else {
			via, err = db.GetOrCreateProjectForWrite(ctx, old, old)
		}
		if err != nil || via.ID != p.ID || via.Key != "space-sim" {
			t.Errorf("%s through the old key: %+v %v", name, via, err)
		}
	}
	if projects, _ := db.ListProjects(ctx); len(projects) != 1 || projects[0].Key != "space-sim" {
		t.Errorf("projects after the rename: %+v", projects)
	}
	if itemProject(t, db, it.ID) != p.ID {
		t.Errorf("the item lost its project")
	}
	if key, ok, err := db.ProjectKeyForAlias(ctx, old); err != nil || !ok || key != "space-sim" {
		t.Errorf("ProjectKeyForAlias: %q %v %v", key, ok, err)
	}
	if _, ok, _ := db.ProjectKeyForAlias(ctx, "never-existed"); ok {
		t.Errorf("an unknown alias resolved")
	}
	if aliases, _ := db.ProjectAliases(ctx, p.ID); len(aliases) != 1 || aliases[0] != old {
		t.Errorf("aliases: %v", aliases)
	}
	rows := auditRows(t, db, "project", p.ID)
	last := rows[len(rows)-1]
	if last.Operation != "key_renamed" || !strings.Contains(last.Detail.String, `"to_key":"space-sim"`) {
		t.Errorf("audit row: %+v", last)
	}
}

func TestRenameProjectKeyAddressedByAnOldKeyAndChained(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "a")
	if _, err := db.RenameProjectKey(ctx, "a", "b"); err != nil {
		t.Fatal(err)
	}
	// Renaming by the old key works too, and every earlier key still resolves.
	if got, err := db.RenameProjectKey(ctx, "a", "c"); err != nil || got.Key != "c" {
		t.Fatalf("rename through an old key: %+v %v", got, err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if via, err := db.GetOrCreateProject(ctx, k, k); err != nil || via.ID != p.ID {
			t.Errorf("key %q: %+v %v", k, via, err)
		}
	}
	if projects, _ := db.ListProjects(ctx); len(projects) != 1 {
		t.Errorf("projects: %+v", projects)
	}
}

func TestRenameProjectKeyBackReleasesTheAlias(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "a")
	db.RenameProjectKey(ctx, "a", "b")
	got, err := db.RenameProjectKey(ctx, "b", "a")
	if err != nil || got.Key != "a" || got.ID != p.ID {
		t.Fatalf("rename back: %+v %v", got, err)
	}
	aliases, _ := db.ProjectAliases(ctx, p.ID)
	if len(aliases) != 1 || aliases[0] != "b" {
		t.Errorf("aliases after renaming back: %v (want only b)", aliases)
	}
	if via, _ := db.GetOrCreateProject(ctx, "b", "b"); via.ID != p.ID {
		t.Errorf("b should still resolve to the project")
	}
}

func TestRenameProjectKeyRefusals(t *testing.T) {
	db := openTest(t)
	a, b := mustProject(t, db, "a"), mustProject(t, db, "b")
	db.RenameProjectKey(ctx, "b", "b2") // b is now an alias of project b2

	cases := map[string]string{
		"b":                       "already used", // another project's old key
		"b2":                      "already used", // another project's live key
		"":                        "cannot be empty",
		" padded":                 "start or end",
		"two\nlines":              "control character",
		strings.Repeat("k", 1025): "at most",
	}
	for k, want := range cases {
		if _, err := db.RenameProjectKey(ctx, "a", k); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("new key %q: err = %v, want %q", k, err, want)
		}
	}
	if _, err := db.RenameProjectKey(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	if got, _ := db.GetOrCreateProject(ctx, "a", "a"); got.ID != a.ID || got.Key != "a" {
		t.Errorf("a refused rename changed the project: %+v", got)
	}
	_ = b

	// The same key writes nothing.
	before := len(auditRows(t, db, "project", a.ID))
	if _, err := db.RenameProjectKey(ctx, "a", "a"); err != nil {
		t.Fatal(err)
	}
	if len(auditRows(t, db, "project", a.ID)) != before {
		t.Errorf("an unchanged rename wrote an audit row")
	}

	// A deleted project is surfaced, not renamed.
	if err := db.SoftDelete(ctx, "project", a.ID); err != nil {
		t.Fatal(err)
	}
	var sd *ErrProjectSoftDeleted
	if _, err := db.RenameProjectKey(ctx, "a", "z"); !errors.As(err, &sd) {
		t.Errorf("deleted project: %v", err)
	}
}

func TestMoveCanNameTheTargetByAnOldKey(t *testing.T) {
	db := openTest(t)
	a, b := mustProject(t, db, "a"), mustProject(t, db, "b")
	db.RenameProjectKey(ctx, "b", "b-new")
	it := mustItem(t, db, a.ID, "t")
	res, err := db.MoveItem(ctx, it.ID, "b", MoveOptions{})
	if err != nil || res.ToProject != "b-new" || itemProject(t, db, it.ID) != b.ID {
		t.Errorf("move by old key: %+v %v", res, err)
	}
}

func TestAliasTableIsAddedToAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db := openAt(t, path)
	mustProject(t, db, "p")
	if _, err := db.conn.Exec(`DROP TABLE project_aliases`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	again := openAt(t, path) // an older database has no such table: opening adds it
	if _, err := again.RenameProjectKey(ctx, "p", "q"); err != nil {
		t.Fatalf("rename on a migrated database: %v", err)
	}
	if via, _ := again.GetOrCreateProject(ctx, "p", "p"); via.Key != "q" {
		t.Errorf("alias after migration: %+v", via)
	}
}
