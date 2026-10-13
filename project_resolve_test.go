package ledgercore

import (
	"errors"
	"strings"
	"testing"
)

func projectCount(t *testing.T, db *DB) int {
	t.Helper()
	return countWhere(t, db, `SELECT COUNT(*) FROM projects`)
}

func TestResolveProjectTriesKeyOldKeyIdAndUniqueName(t *testing.T) {
	db := openTest(t)
	path := "/Users/me/Photon Datum/space_sim"
	sim := mustProject(t, db, path)
	if _, err := db.RenameProject(ctx, path, "Space Sim"); err != nil {
		t.Fatal(err)
	}
	other := mustProject(t, db, "other")
	if _, err := db.RenameProjectKey(ctx, "other", "other-2"); err != nil {
		t.Fatal(err)
	}
	before := projectCount(t, db)

	cases := []struct {
		what, ref string
		want      string // the project id it must resolve to
	}{
		{"its key", path, sim.ID},
		{"its id", sim.ID, sim.ID},
		{"its display name", "Space Sim", sim.ID},
		{"its display name, any case", "space sim", sim.ID},
		{"an old key", "other", other.ID},
		{"the new key after a rename", "other-2", other.ID},
		{"its id, with stray spaces around it", "  " + sim.ID + " ", sim.ID},
	}
	for _, c := range cases {
		got, err := db.ResolveProject(ctx, c.ref)
		if err != nil || got.ID != c.want {
			t.Errorf("%s (%q): %+v %v", c.what, c.ref, got, err)
		}
	}
	if projectCount(t, db) != before {
		t.Errorf("resolving created a project: %d -> %d", before, projectCount(t, db))
	}
}

func TestResolveProjectUnknownIsAnErrorThatNamesTheLikelyOnesAndCreatesNothing(t *testing.T) {
	db := openTest(t)
	path := "/Users/me/Photon Datum/space_sim"
	mustProject(t, db, path)
	db.RenameProject(ctx, path, "Space Sim")
	mustProject(t, db, "cortex-stack")
	before := projectCount(t, db)

	// The two things the small model really passed on 2026-10-12: the old short name, and an invented id.
	for _, ref := range []string{"space_sim", "Space-Sim", "8a2f9b1c-7d1e-4c5b-9e3f-2a1d8c6e5f7a", "nothing at all"} {
		got, err := db.ResolveProject(ctx, ref)
		var nf *ProjectNotFoundError
		if got != nil || !errors.As(err, &nf) || !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q: %+v %v", ref, got, err)
		}
		if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "cortex-stack") {
			t.Errorf("%q: the error should say it is missing and list the projects: %v", ref, err)
		}
	}
	// A near miss is named, with its display name: this is what lets the model retry correctly.
	_, err := db.ResolveProject(ctx, "space_sim")
	if !strings.Contains(err.Error(), `Did you mean: "/Users/me/Photon Datum/space_sim" (named "Space Sim")?`) {
		t.Errorf("no suggestion: %v", err)
	}
	// An id that matches nothing is not a near miss for anything.
	_, err = db.ResolveProject(ctx, "8a2f9b1c-7d1e-4c5b-9e3f-2a1d8c6e5f7a")
	if strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("an invented id must not get a suggestion: %v", err)
	}
	if projectCount(t, db) != before {
		t.Errorf("a failed lookup created projects: %d -> %d", before, projectCount(t, db))
	}
}

func TestResolveProjectAmbiguousNameIsRefusedNotGuessed(t *testing.T) {
	db := openTest(t)
	a, b := mustProject(t, db, "alpha"), mustProject(t, db, "beta")
	db.RenameProject(ctx, "alpha", "Shared Name")
	db.RenameProject(ctx, "beta", "shared name")
	_ = a
	_ = b
	_, err := db.ResolveProject(ctx, "Shared Name")
	if err == nil || !strings.Contains(err.Error(), "matches several projects") || !strings.Contains(err.Error(), `"alpha"`) || !strings.Contains(err.Error(), `"beta"`) {
		t.Errorf("ambiguous name: %v", err)
	}
	// Their keys still work, and the key wins over a name that happens to equal it.
	if got, err := db.ResolveProject(ctx, "alpha"); err != nil || got.ID != a.ID {
		t.Errorf("key: %+v %v", got, err)
	}
}

func TestResolveProjectKeyBeatsAnotherProjectsName(t *testing.T) {
	db := openTest(t)
	one := mustProject(t, db, "one")
	two := mustProject(t, db, "two")
	db.RenameProject(ctx, "two", "one") // two's DISPLAY name is one's KEY
	got, err := db.ResolveProject(ctx, "one")
	if err != nil || got.ID != one.ID {
		t.Errorf("the key must win over a name: %+v %v (two is %s)", got, err, two.ID)
	}
}

func TestResolveProjectDeletedIsStillReadableButNotWritable(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "gone")
	mustItem(t, db, p.ID, "kept")
	if err := db.SoftDelete(ctx, "project", p.ID); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"gone", p.ID} {
		if got, err := db.ResolveProject(ctx, ref); err != nil || got.ID != p.ID {
			t.Errorf("a deleted project's data stays readable (%q): %+v %v", ref, got, err)
		}
		var sd *ErrProjectSoftDeleted
		if _, err := db.ResolveProjectForWrite(ctx, ref); !errors.As(err, &sd) {
			t.Errorf("writing to a deleted project (%q) must say so: %v", ref, err)
		}
	}
	// A deleted project's display name is not claimed by a live reference.
	db.RenameProject(ctx, "gone", "Ghost") // refused for a deleted project; ignore the error
	if _, err := db.ResolveProject(ctx, "Ghost"); err == nil {
		t.Errorf("a name must not resolve to a deleted project")
	}
}

func TestResolveProjectForWriteReportsNotFoundSoTheCallerCanCreate(t *testing.T) {
	db := openTest(t)
	mustProject(t, db, "real")
	_, err := db.ResolveProjectForWrite(ctx, "brand-new")
	var nf *ProjectNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("a write to a new key must see not-found (it then creates): %v", err)
	}
	if got, err := db.ResolveProjectForWrite(ctx, "real"); err != nil || got.Key != "real" {
		t.Errorf("an existing one resolves: %+v %v", got, err)
	}
	if _, err := db.ResolveProject(ctx, ""); err == nil {
		t.Errorf("an empty reference is an error")
	}
}

func TestProjectNotFoundMessageIsBoundedForALargeLedger(t *testing.T) {
	db := openTest(t)
	for i := 0; i < 40; i++ {
		mustProject(t, db, "project-"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	_, err := db.ResolveProject(ctx, "zzz-nothing")
	if err == nil || !strings.Contains(err.Error(), "and 25 more") {
		t.Errorf("the key list should be capped at 15 with a count: %v", err)
	}
}

func TestResolveProjectAnEmptyDeletedProjectDoesNotShadowTheRealOne(t *testing.T) {
	db := openTest(t)
	// What the 2026-10-12 cleanup left behind: an empty, soft-deleted project keyed with a model's short name.
	junk := mustProject(t, db, "space_sim")
	if err := db.SoftDelete(ctx, "project", junk.ID); err != nil {
		t.Fatal(err)
	}
	real := mustProject(t, db, "/Users/me/Photon Datum/space_sim")
	db.RenameProject(ctx, "/Users/me/Photon Datum/space_sim", "Space Sim")

	for _, ref := range []string{"space_sim", junk.ID} {
		got, err := db.ResolveProject(ctx, ref)
		var nf *ProjectNotFoundError
		if got != nil || !errors.As(err, &nf) {
			t.Fatalf("%q: an empty deleted project must count as not found: %+v %v", ref, got, err)
		}
	}
	_, err := db.ResolveProject(ctx, "space_sim")
	if !strings.Contains(err.Error(), `"/Users/me/Photon Datum/space_sim" (named "Space Sim")`) {
		t.Errorf("the real project should be suggested: %v", err)
	}
	if got, err := db.ResolveProject(ctx, real.ID); err != nil || got.ID != real.ID {
		t.Errorf("the real one still resolves: %+v %v", got, err)
	}
	// Anything left in a deleted project makes it readable again: even one old project-level note.
	if _, err := db.conn.Exec(`INSERT INTO notes (id, project_id, item_id, type, body, created_at, updated_at) VALUES ('n1', ?, NULL, 'comment', 'old note', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, junk.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := db.ResolveProject(ctx, "space_sim"); err != nil || got.ID != junk.ID {
		t.Errorf("a deleted project that holds data stays readable: %+v %v", got, err)
	}
}
