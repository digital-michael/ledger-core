package ledgercore

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Labels are zero-or-more per item, unique per item (case-insensitive,
// trimmed), enforced by the database. See ledger item 20a5d442.

func labelsOf(t *testing.T, db *DB, itemID string) []string {
	t.Helper()
	ls, err := db.ListLabels(ctx, itemID)
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	out := []string{}
	for _, l := range ls {
		out = append(out, l.Label)
	}
	return out
}

func TestLabels_createWithSingularAndPluralAndCompat(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")

	one, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "one", Label: "core"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one.Labels, []string{"core"}) || one.Label.String != "core" {
		t.Errorf("single: Labels=%v Label=%q", one.Labels, one.Label.String)
	}
	// A new database never has the legacy column at all.
	if has, err := hasColumn(db.conn, "items", "label"); err != nil || has {
		t.Errorf("a fresh database should have no items.label: has=%v err=%v", has, err)
	}

	many, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "many", Labels: []string{"b", "A", " a ", "c"}})
	if err != nil {
		t.Fatal(err)
	}
	// Duplicates (case-insensitive, trimmed) collapse; order is by label.
	if !reflect.DeepEqual(many.Labels, []string{"A", "b", "c"}) {
		t.Errorf("plural: Labels=%v", many.Labels)
	}

	if _, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "both", Label: "x", Labels: []string{"y"}}); err == nil {
		t.Error("label and labels together must be refused, not silently picked between")
	}
	none := mustItem(t, db, p.ID, "none")
	if len(none.Labels) != 0 || none.Label.Valid {
		t.Errorf("no labels: Labels=%v Label=%v", none.Labels, none.Label)
	}
}

func TestLabels_addIsUniquePerItem(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	it := mustItem(t, db, p.ID, "t")

	l, added, err := db.AddLabel(ctx, it.ID, "  Feature ")
	if err != nil || !added || l.Label != "Feature" {
		t.Fatalf("first add: %+v %v %v", l, added, err)
	}
	for _, dup := range []string{"Feature", "feature", "FEATURE", " feature  "} {
		again, added, err := db.AddLabel(ctx, it.ID, dup)
		if err != nil || added || again.ID != l.ID {
			t.Errorf("adding %q again: added=%v id=%v err=%v; want a no-op returning the existing label", dup, added, again, err)
		}
	}
	if got := labelsOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"Feature"}) {
		t.Errorf("labels = %v", got)
	}
	// Another item may hold the same label.
	other := mustItem(t, db, p.ID, "o")
	if _, added, err := db.AddLabel(ctx, other.ID, "feature"); err != nil || !added {
		t.Errorf("same label on another item: %v %v", added, err)
	}
}

func TestLabels_databaseEnforcesUniqueness(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	it := mustItem(t, db, p.ID, "t")
	if _, _, err := db.AddLabel(ctx, it.ID, "Core"); err != nil {
		t.Fatal(err)
	}
	// Bypass the package: the index alone must hold.
	_, err := db.conn.Exec(`INSERT INTO item_labels (id, item_id, label, created_at) VALUES ('x', ?, 'core', '2026-01-01T00:00:00Z')`, it.ID)
	if err == nil {
		t.Fatal("a case-variant duplicate was inserted behind the tool layer")
	}
	// A soft-deleted duplicate does not count.
	if _, err := db.conn.Exec(`INSERT INTO item_labels (id, item_id, label, created_at, deleted_at) VALUES ('y', ?, 'core', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')`, it.ID); err != nil {
		t.Errorf("a soft-deleted duplicate should be allowed: %v", err)
	}
}

func TestLabels_invalidValuesRefused(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	it := mustItem(t, db, p.ID, "t")
	for _, bad := range []string{"", "   ", "a\nb", "a\tb", strings.Repeat("x", maxLabelLen+1)} {
		if _, _, err := db.AddLabel(ctx, it.ID, bad); err == nil {
			t.Errorf("label %q was accepted", bad)
		}
	}
	if _, _, err := db.AddLabel(ctx, "nonexistent", "x"); err == nil {
		t.Error("unknown item accepted")
	}
}

func TestLabels_removeByValueAndReAdd(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	it := mustItem(t, db, p.ID, "t")
	for _, l := range []string{"a", "b"} {
		if _, _, err := db.AddLabel(ctx, it.ID, l); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := db.RemoveLabel(ctx, it.ID, "  A ")
	if err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if got := labelsOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("after remove: %v", got)
	}
	if removed, err := db.RemoveLabel(ctx, it.ID, "a"); err != nil || removed {
		t.Errorf("removing an absent label should report false, not fail: %v %v", removed, err)
	}
	// The partial unique index lets a removed label come back.
	if _, added, err := db.AddLabel(ctx, it.ID, "a"); err != nil || !added {
		t.Errorf("re-add after remove: %v %v", added, err)
	}
	// Removal is a soft delete: the row and its audit remain.
	var n int
	db.conn.QueryRow(`SELECT COUNT(*) FROM item_labels WHERE item_id = ? AND deleted_at IS NOT NULL`, it.ID).Scan(&n)
	if n != 1 {
		t.Errorf("soft-deleted label rows = %d, want 1", n)
	}
}

func TestLabels_auditAndUpdatedAt(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	it := mustItem(t, db, p.ID, "t")
	before, _ := db.GetItem(ctx, it.ID)

	l, _, err := db.AddLabel(ctx, it.ID, "x")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetItem(ctx, it.ID)
	if after.UpdatedAt == before.UpdatedAt {
		t.Error("adding a label should move the item's updated_at (optimistic concurrency)")
	}
	rows := auditRows(t, db, "item_label", l.ID)
	if len(rows) != 1 || rows[0].Operation != "created" || !strings.Contains(rows[0].Detail.String, `"label":"x"`) {
		t.Errorf("add audit: %+v", rows)
	}
	if !rows[0].Actor.Valid {
		t.Error("audit row has no actor")
	}
	if _, err := db.RemoveLabel(ctx, it.ID, "x"); err != nil {
		t.Fatal(err)
	}
	rows = auditRows(t, db, "item_label", l.ID)
	if len(rows) != 2 || rows[1].Operation != "deleted" {
		t.Errorf("remove audit: %+v", rows)
	}
	// An unchanged add writes nothing.
	db.AddLabel(ctx, it.ID, "y")
	n := len(auditRows(t, db, "item", it.ID))
	db.AddLabel(ctx, it.ID, "Y")
	if len(auditRows(t, db, "item", it.ID)) != n {
		t.Error("a no-op add wrote audit")
	}
}

func TestLabels_updateItemLabelReplacesTheWholeSet(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	it, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "t", Labels: []string{"a", "b", "c"}})

	got, err := db.UpdateItem(ctx, it.ID, UpdateItemParams{Label: ptr("b")})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Labels, []string{"b"}) {
		t.Errorf("replace -> %v", got.Labels)
	}
	// Setting to the same single value is a no-op.
	n := len(auditRows(t, db, "item", it.ID))
	if _, err := db.UpdateItem(ctx, it.ID, UpdateItemParams{Label: ptr("B")}); err != nil {
		t.Fatal(err)
	}
	if len(auditRows(t, db, "item", it.ID)) != n {
		t.Error("no-op label update wrote audit")
	}
	// Empty clears all.
	got, err = db.UpdateItem(ctx, it.ID, UpdateItemParams{Label: ptr("")})
	if err != nil || len(got.Labels) != 0 || got.Label.Valid {
		t.Errorf("clear -> %v %v %v", got, err, got.Labels)
	}
	// The change is audited as an item update carrying from/to.
	found := false
	for _, r := range auditRows(t, db, "item", it.ID) {
		if r.Operation == "updated" && strings.Contains(r.Detail.String, `"label"`) {
			found = true
		}
	}
	if !found {
		t.Error("label update left no 'updated' audit row")
	}
	// A label-only change still reports the item, and with a conflict token works.
	cur, _ := db.GetItem(ctx, it.ID)
	if _, err := db.UpdateItemFields(ctx, it.ID, ItemUpdate{Fields: ItemFields{Label: ptr("z")}, ExpectedUpdatedAt: cur.UpdatedAt}); err != nil {
		t.Errorf("label edit with a conflict token: %v", err)
	}
	if _, err := db.UpdateItemFields(ctx, it.ID, ItemUpdate{Fields: ItemFields{Label: ptr("w")}, ExpectedUpdatedAt: cur.UpdatedAt}); err == nil {
		t.Error("a stale token should conflict")
	}
}

func TestLabels_listItemsFilterUsesTheTable(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	a, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "a", Labels: []string{"x", "y"}})
	b, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "b", Labels: []string{"y"}})
	mustItem(t, db, p.ID, "c")

	got, err := db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Label: "X"})
	if err != nil || len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("filter x: %v %v", got, err)
	}
	got, _ = db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Label: "y"})
	if len(got) != 2 {
		t.Errorf("filter y: %d items, want 2", len(got))
	}
	if !reflect.DeepEqual(got[0].Labels, []string{"x", "y"}) || !reflect.DeepEqual(got[1].Labels, []string{"y"}) {
		t.Errorf("listed items must carry all their labels: %v %v", got[0].Labels, got[1].Labels)
	}
	// A removed label no longer matches.
	db.RemoveLabel(ctx, b.ID, "y")
	got, _ = db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Label: "y"})
	if len(got) != 1 {
		t.Errorf("after remove: %d items, want 1", len(got))
	}
	// Find and search also carry labels.
	found, _ := db.FindItems(ctx, p.ID, "a")
	if len(found) == 0 || len(found[0].Labels) != 2 {
		t.Errorf("FindItems labels: %+v", found)
	}
}

func TestLabels_replaceAcrossItems(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	q := mustProject(t, db, "other")
	a, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "a", Labels: []string{"required", "x"}})
	b, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: q.ID, Title: "b", Labels: []string{"Required"}})
	c, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "c", Labels: []string{"required", "nice-to-have"}})
	d, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "d", Labels: []string{"keep"}})

	n, err := db.ReplaceLabels(ctx, "REQUIRED", "nice-to-have")
	if err != nil || n != 3 {
		t.Fatalf("replace: n=%d err=%v", n, err)
	}
	want := map[string][]string{
		a.ID: {"nice-to-have", "x"},
		b.ID: {"nice-to-have"},
		c.ID: {"nice-to-have"}, // already had it: the old one is removed, never duplicated
		d.ID: {"keep"},
	}
	for id, w := range want {
		if got := labelsOf(t, db, id); !reflect.DeepEqual(got, w) {
			t.Errorf("%s: %v, want %v", id[:8], got, w)
		}
	}
	// All rows of one replace share a batch id.
	batches := map[string]bool{}
	for _, id := range []string{a.ID, b.ID, c.ID} {
		for _, r := range auditRows(t, db, "item", id) {
			_ = r
		}
	}
	rows, _ := db.ListAuditLog(ctx, AuditFilter{EntityType: "item_label"})
	for _, r := range rows {
		if r.Batch.Valid {
			batches[r.Batch.String] = true
		}
	}
	if len(batches) != 1 {
		t.Errorf("a replace should be one batch, saw %d", len(batches))
	}
	if _, err := db.ReplaceLabels(ctx, "", "x"); err == nil {
		t.Error("empty existing label accepted")
	}
	if _, err := db.ReplaceLabels(ctx, "x", " "); err == nil {
		t.Error("empty new label accepted")
	}
	if n, err := db.ReplaceLabels(ctx, "nothing-has-this", "z"); err != nil || n != 0 {
		t.Errorf("replace of an unused label: %d %v", n, err)
	}
}

func TestLabels_distinctWithCounts(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	q := mustProject(t, db, "other")
	db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "1", Labels: []string{"Feature", "ui"}})
	db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "2", Labels: []string{"feature"}})
	gone, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "3", Labels: []string{"ghost"}})
	db.CreateItem(ctx, CreateItemParams{ProjectID: q.ID, Title: "4", Labels: []string{"ui"}})
	if err := db.SoftDelete(ctx, "item", gone.ID); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListDistinctLabels(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, l := range all {
		got[strings.ToLower(l.Label)] = l.Count
	}
	if !reflect.DeepEqual(got, map[string]int{"feature": 2, "ui": 2}) {
		t.Errorf("all projects: %v (deleted items must not count)", got)
	}
	if all[0].Label > all[1].Label {
		t.Errorf("not sorted: %v", all)
	}
	scoped, _ := db.ListDistinctLabels(ctx, []string{q.ID})
	if len(scoped) != 1 || scoped[0].Label != "ui" || scoped[0].Count != 1 {
		t.Errorf("scoped: %+v", scoped)
	}
}

func TestLabels_deletedItemIsReadOnly(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	it, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "t", Labels: []string{"a"}})
	db.SoftDelete(ctx, "item", it.ID)
	if _, _, err := db.AddLabel(ctx, it.ID, "b"); err == nil {
		t.Error("AddLabel on a deleted item")
	}
	if _, err := db.RemoveLabel(ctx, it.ID, "a"); err == nil {
		t.Error("RemoveLabel on a deleted item")
	}
	if _, err := db.UpdateItem(ctx, it.ID, UpdateItemParams{Label: ptr("z")}); err == nil {
		t.Error("label update on a deleted item")
	}
}

func TestLabels_softDeleteAndRestoreAsEntity(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	it := mustItem(t, db, p.ID, "t")
	l, _, _ := db.AddLabel(ctx, it.ID, "a")
	if err := db.SoftDelete(ctx, "item_label", l.ID); err != nil {
		t.Fatalf("item_label must be a deletable entity: %v", err)
	}
	if got := labelsOf(t, db, it.ID); len(got) != 0 {
		t.Errorf("after delete: %v", got)
	}
	if err := db.Restore(ctx, "item_label", l.ID); err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("after restore: %v", got)
	}
	// Restoring when a live duplicate exists is refused loudly, never silently doubled.
	db.SoftDelete(ctx, "item_label", l.ID)
	db.AddLabel(ctx, it.ID, "A")
	if err := db.Restore(ctx, "item_label", l.ID); err == nil {
		t.Error("restoring over a live duplicate should fail")
	}
	if got := labelsOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"A"}) {
		t.Errorf("duplicate slipped in: %v", got)
	}
}

// The migration: existing items.label values are copied into item_labels
// exactly once. Nothing is lost, nothing is doubled, and a label removed
// afterwards does not come back on the next open.
func TestLabels_migrationCopiesEveryLegacyLabelOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db := openAt(t, path)
	withLegacyLabelColumn(t, db)
	p := mustProject(t, db, "lab")
	var ids []string
	legacy := []string{"alpha", "  padded  ", "Beta", ""}
	for i, l := range legacy {
		it := mustItem(t, db, p.ID, "item"+string(rune('a'+i)))
		ids = append(ids, it.ID)
		if l != "" {
			if _, err := db.conn.Exec(`UPDATE items SET label = ? WHERE id = ?`, l, it.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A soft-deleted item's label is copied as well: no loss, deleted or not.
	del := mustItem(t, db, p.ID, "deleted")
	db.conn.Exec(`UPDATE items SET label = 'from-deleted' WHERE id = ?`, del.ID)
	db.SoftDelete(ctx, "item", del.ID)
	db.Close()

	db = openAt(t, path) // first open after "upgrade" runs the copy
	count := func() int {
		var n int
		db.conn.QueryRow(`SELECT COUNT(*) FROM item_labels`).Scan(&n)
		return n
	}
	var legacyCount int
	db.conn.QueryRow(`SELECT COUNT(*) FROM items WHERE label IS NOT NULL AND TRIM(label) <> ''`).Scan(&legacyCount)
	if legacyCount != 4 || count() != legacyCount {
		t.Fatalf("legacy=%d copied=%d; the zero-loss check failed", legacyCount, count())
	}
	if got := labelsOf(t, db, ids[1]); !reflect.DeepEqual(got, []string{"padded"}) {
		t.Errorf("padded label: %v", got)
	}
	if got := labelsOf(t, db, del.ID); !reflect.DeepEqual(got, []string{"from-deleted"}) {
		t.Errorf("deleted item's label: %v", got)
	}
	if got := labelsOf(t, db, ids[3]); len(got) != 0 {
		t.Errorf("unlabeled item gained %v", got)
	}
	// The legacy column is left alone.
	var still int
	db.conn.QueryRow(`SELECT COUNT(*) FROM items WHERE label IS NOT NULL`).Scan(&still)
	if still != 4 {
		t.Errorf("items.label was modified: %d", still)
	}

	// Idempotent: reopen twice, nothing changes.
	db.Close()
	db = openAt(t, path)
	db.Close()
	db = openAt(t, path)
	if count() != 4 {
		t.Errorf("reopening changed the count to %d", count())
	}

	// A removal survives reopening: the stale legacy value must not resurrect it.
	if _, err := db.RemoveLabel(ctx, ids[0], "alpha"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = openAt(t, path)
	if got := labelsOf(t, db, ids[0]); len(got) != 0 {
		t.Errorf("a removed label came back: %v", got)
	}
	// And so does a replace-all that cleared an item.
	if _, err := db.UpdateItem(ctx, ids[2], UpdateItemParams{Label: ptr("")}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = openAt(t, path)
	if got := labelsOf(t, db, ids[2]); len(got) != 0 {
		t.Errorf("a cleared label came back: %v", got)
	}
}

func TestLabels_legacyCountHelperReportsMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db := openAt(t, path)
	withLegacyLabelColumn(t, db)
	p := mustProject(t, db, "lab")
	a := mustItem(t, db, p.ID, "a")
	mustItem(t, db, p.ID, "b")
	db.conn.Exec(`UPDATE items SET label = 'x' WHERE id = ?`, a.ID) // legacy value with no copy yet

	rep, err := db.LabelMigrationReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || len(rep.Projects) != 1 || rep.Projects[0].Legacy != 1 || rep.Projects[0].Copied != 0 {
		t.Errorf("report should flag the uncopied label: %+v", rep)
	}
	db.Close()
	db = openAt(t, path)
	rep, _ = db.LabelMigrationReport(ctx)
	if !rep.OK() {
		t.Errorf("after the copy the gate should pass: %+v", rep)
	}
}

func TestLabels_updateFieldsLabelsSetsTheExactList(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "lab")
	it, _ := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "t", Labels: []string{"a", "b", "c"}})

	set := func(ls ...string) (*Item, error) {
		return db.UpdateItemFields(ctx, it.ID, ItemUpdate{Fields: ItemFields{Labels: &ls}})
	}
	before, _ := db.ListLabels(ctx, it.ID)
	var bID string
	for _, r := range before {
		if r.Label == "b" {
			bID = r.ID
		}
	}
	got, err := set("b", "D", "d", " e ")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Labels, []string{"b", "D", "e"}) {
		t.Errorf("exact set -> %v", got.Labels)
	}
	// "b" was kept, not removed and re-added: it is the same row.
	after, _ := db.ListLabels(ctx, it.ID)
	kept := false
	for _, r := range after {
		if r.Label == "b" && r.ID == bID && bID != "" {
			kept = true
		}
	}
	if !kept {
		t.Error("a label that stayed in the set was rewritten")
	}
	// The same set again is a no-op: no new audit rows on the item.
	n := len(auditRows(t, db, "item", it.ID))
	if _, err := set("e", "d", "B"); err != nil {
		t.Fatal(err)
	}
	if len(auditRows(t, db, "item", it.ID)) != n {
		t.Error("an unchanged set wrote audit")
	}
	// Empty clears all.
	if got, err := set(); err != nil || len(got.Labels) != 0 {
		t.Errorf("clear -> %v %v", got.Labels, err)
	}
	// A bad entry rejects the whole edit and changes nothing.
	set("keep")
	if _, err := set("ok", ""); err == nil {
		t.Error("an empty entry should be refused")
	}
	if got := labelsOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"keep"}) {
		t.Errorf("a refused edit changed the labels: %v", got)
	}
	// label and labels together are refused.
	l := "x"
	ls := []string{"y"}
	if _, err := db.UpdateItemFields(ctx, it.ID, ItemUpdate{Fields: ItemFields{Label: &l, Labels: &ls}}); err == nil {
		t.Error("label with labels accepted")
	}
	// Combined with another field, one save is one batch.
	title := "renamed"
	ls2 := []string{"z"}
	if _, err := db.UpdateItemFields(ctx, it.ID, ItemUpdate{Fields: ItemFields{Title: &title, Labels: &ls2}}); err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(t, db, it.ID); !reflect.DeepEqual(got, []string{"z"}) {
		t.Errorf("combined edit: %v", got)
	}
}

// withLegacyLabelColumn recreates the pre-migration shape for a test: an
// items.label column an older release would have left behind.
func withLegacyLabelColumn(t *testing.T, db *DB) {
	t.Helper()
	if err := ensureColumn(db.conn, "items", "label", "TEXT"); err != nil {
		t.Fatal(err)
	}
}

func TestLabels_dropRefusesWhileAnyLegacyLabelIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db := openAt(t, path)
	withLegacyLabelColumn(t, db)
	p := mustProject(t, db, "lab")
	a := mustItem(t, db, p.ID, "a")
	db.conn.Exec(`UPDATE items SET label = 'only-here' WHERE id = ?`, a.ID) // never copied

	dropped, err := db.DropLegacyLabelColumn(ctx)
	if err == nil || dropped {
		t.Fatalf("the drop went ahead with a label missing: %v %v", dropped, err)
	}
	wantErrContaining(t, err, "zero-loss check failed")
	wantErrContaining(t, err, "lab: 1 legacy labels, 0 in item_labels")
	if has, _ := hasColumn(db.conn, "items", "label"); !has {
		t.Error("a refused drop removed the column anyway")
	}
	var v string
	db.conn.QueryRow(`SELECT label FROM items WHERE id = ?`, a.ID).Scan(&v)
	if v != "only-here" {
		t.Errorf("the legacy value was disturbed: %q", v)
	}
}

func TestLabels_dropAfterCopyKeepsEverythingWorking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	db := openAt(t, path)
	withLegacyLabelColumn(t, db)
	p := mustProject(t, db, "lab")
	q := mustProject(t, db, "other")
	var ids []string
	for i, l := range []string{"alpha", "Beta", "gamma"} {
		it := mustItem(t, db, map[bool]string{true: p.ID, false: q.ID}[i < 2], "item-"+l)
		db.conn.Exec(`UPDATE items SET label = ? WHERE id = ?`, l, it.ID)
		ids = append(ids, it.ID)
	}
	db.Close()

	db = openAt(t, path) // the copy runs
	rep, err := db.LabelMigrationReport(ctx)
	if err != nil || !rep.OK() || rep.Legacy != 3 {
		t.Fatalf("gate before the drop: %+v %v", rep, err)
	}
	dropped, err := db.DropLegacyLabelColumn(ctx)
	if err != nil || !dropped {
		t.Fatalf("drop: %v %v", dropped, err)
	}
	if has, _ := hasColumn(db.conn, "items", "label"); has {
		t.Fatal("the column is still there")
	}
	// Idempotent.
	if dropped, err := db.DropLegacyLabelColumn(ctx); err != nil || dropped {
		t.Errorf("a second drop: %v %v", dropped, err)
	}
	if rep, err := db.LabelMigrationReport(ctx); err != nil || !rep.OK() || len(rep.Projects) != 0 {
		t.Errorf("report after the drop: %+v %v", rep, err)
	}

	// Every label survived, and every operation still works without the column.
	for i, want := range []string{"alpha", "Beta", "gamma"} {
		if got := labelsOf(t, db, ids[i]); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("item %d labels = %v, want %s", i, got, want)
		}
	}
	it, err := db.CreateItem(ctx, CreateItemParams{ProjectID: p.ID, Title: "after", Labels: []string{"x", "y"}})
	if err != nil || len(it.Labels) != 2 {
		t.Fatalf("create after the drop: %+v %v", it, err)
	}
	if _, err := db.UpdateItem(ctx, it.ID, UpdateItemParams{Label: ptr("z"), Title: ptr("renamed")}); err != nil {
		t.Errorf("update after the drop: %v", err)
	}
	if got, _ := db.ListItems(ctx, ItemFilter{ProjectID: p.ID, Label: "alpha"}); len(got) != 1 {
		t.Errorf("filter after the drop: %d", len(got))
	}
	if got, _ := db.FindItems(ctx, "", "item-"); len(got) != 3 {
		t.Errorf("find after the drop: %d", len(got))
	}
	if _, _, err := db.AddLabel(ctx, ids[0], "more"); err != nil {
		t.Errorf("add label after the drop: %v", err)
	}
	if n, err := db.ReplaceLabels(ctx, "alpha", "ALPHA2"); err != nil || n != 1 {
		t.Errorf("replace after the drop: %d %v", n, err)
	}
	if _, err := db.MoveItem(ctx, ids[0], "other", MoveOptions{}); err != nil {
		t.Errorf("move after the drop: %v", err)
	}
	if _, err := db.SearchItems(ctx, "", "item"); err != nil {
		t.Errorf("search after the drop: %v", err)
	}
	if _, err := db.HealthFindings(ctx); err != nil {
		t.Errorf("health after the drop: %v", err)
	}
	if _, err := db.SummarizeProject(ctx, p.ID, "lab"); err != nil {
		t.Errorf("summary after the drop: %v", err)
	}

	// Reopening does not bring the column back, and nothing is copied again.
	db.Close()
	db = openAt(t, path)
	if has, _ := hasColumn(db.conn, "items", "label"); has {
		t.Error("reopening recreated items.label")
	}
	if got := labelsOf(t, db, ids[2]); !reflect.DeepEqual(got, []string{"gamma"}) {
		t.Errorf("after reopen: %v", got)
	}
	var integrity string
	db.conn.QueryRow(`PRAGMA integrity_check`).Scan(&integrity)
	if integrity != "ok" {
		t.Errorf("integrity_check = %s", integrity)
	}
}
