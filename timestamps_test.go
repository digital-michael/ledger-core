package ledgercore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// Every stamp is fixed-width UTC, so that comparing two of them as text is the
// same as comparing the moments they name. RFC3339Nano trims trailing zeros, which
// made "…:00.5Z" sort BEFORE "…:00Z" although it is later ('.' < 'Z').

var stampRE = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{9}Z$`)

func TestStamp_fixedWidthAndSortsLikeTime(t *testing.T) {
	base := time.Date(2026, 11, 8, 13, 0, 5, 0, time.UTC)
	var times []time.Time
	for _, ns := range []int{0, 1, 100, 123000000, 500000000, 999999999} {
		times = append(times, base.Add(time.Duration(ns)))
	}
	times = append(times, base.Add(time.Second), base.Add(time.Hour), base.AddDate(0, 0, 1), base.AddDate(1, 0, 0))
	var stamps []string
	for _, tm := range times {
		s := FormatStamp(tm)
		if len(s) != 30 || !stampRE.MatchString(s) {
			t.Errorf("%v -> %q: not fixed width", tm, s)
		}
		stamps = append(stamps, s)
	}
	sorted := append([]string(nil), stamps...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(sorted, stamps) {
		t.Errorf("sorting as text changed the order:\n got %v\nwant %v", sorted, stamps)
	}
	// A time in another zone is written as UTC.
	est := time.Date(2026, 11, 8, 8, 0, 5, 0, time.FixedZone("EST", -5*3600))
	if got := FormatStamp(est); got != "2026-11-08T13:00:05.000000000Z" {
		t.Errorf("zone: %q", got)
	}
	if !stampRE.MatchString(nowUTC()) {
		t.Errorf("nowUTC() = %q", nowUTC())
	}
	// And it round-trips.
	back, err := ParseStamp(stamps[4])
	if err != nil || !back.Equal(times[4]) {
		t.Errorf("round trip: %v %v", back, err)
	}
	// The old format is what went wrong: shown here so the reason stays on record.
	old := []string{base.Format(time.RFC3339Nano), base.Add(500 * time.Millisecond).Format(time.RFC3339Nano)}
	if !(old[1] < old[0]) {
		t.Skip("the old format no longer misorders; nothing to demonstrate")
	}
}

// timestampSweep returns every non-null value of every column named *_at, in any
// table, that is not a fixed-width stamp. It discovers tables from the database, so a
// table added later is checked without being listed.
func timestampSweep(t *testing.T, db *DB) []string {
	t.Helper()
	var bad []string
	cols, err := timestampColumns(ctx, db.conn.d, db.conn)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		rows, err := db.conn.Query(fmt.Sprintf(`SELECT %s FROM %s WHERE %s IS NOT NULL`, c.column, c.table, c.column))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			rows.Scan(&v)
			if !stampRE.MatchString(v) {
				bad = append(bad, fmt.Sprintf("%s.%s = %q", c.table, c.column, v))
			}
		}
		rows.Close()
	}
	return bad
}

// Every writer in the package, exercised once; then every *_at column in the
// database is checked. A new table or a new writer that forgets the format fails here.
func TestStamp_everyWriterUsesTheFixedFormat(t *testing.T) {
	db := openAs(t, filepath.Join(t.TempDir(), "l.db"), alice, nil)
	p := mustProject(t, db, "demo")
	a := mustItem(t, db, p.ID, "a")
	b := mustItem(t, db, p.ID, "b")
	db.UpdateItem(ctx, a.ID, UpdateItemParams{Title: ptr("renamed"), Label: ptr("x")})
	db.UpdateItemStatus(ctx, a.ID, "in_progress")
	db.AddLabel(ctx, a.ID, "y")
	db.RemoveLabel(ctx, a.ID, "y")
	db.AddAssignee(ctx, a.ID, "person:bob")
	db.RemoveAssignee(ctx, a.ID, "person:bob")
	db.AddWatcher(ctx, a.ID, "person:bob")
	db.RemoveWatcher(ctx, a.ID, "person:bob")
	db.AddProjectWatcher(ctx, p.ID, "person:bob")
	db.RemoveProjectWatcher(ctx, p.ID, "person:bob")
	db.AddProjectMember(ctx, p.ID, "person:bob", "member")
	db.SetProjectMemberRole(ctx, p.ID, "person:bob", "viewer")
	db.RemoveProjectMember(ctx, p.ID, "person:bob")
	db.TransferItemOwner(ctx, a.ID, "person:carol")
	note, _ := db.AddNote(ctx, AddNoteParams{ItemID: a.ID, Body: "hello"})
	db.UpdateNote(ctx, note.ID, UpdateNoteParams{Body: ptr("edited")})
	db.StartTimer(ctx, a.ID)
	db.StopTimer(ctx, a.ID)
	res, _ := db.AddResource(ctx, p.ID, a.ID, "https://x.test", "x")
	db.UpdateResource(ctx, res.ID, UpdateResourceParams{Label: ptr("y")})
	db.RelateItems(ctx, a.ID, b.ID, "related_to")
	db.RenameProjectKey(ctx, p.ID, "demo2")
	db.SoftDelete(ctx, "item", b.ID)
	db.Restore(ctx, "item", b.ID)
	wf := mustWorkflow(t, db, p.ID, sampleDoc())
	_ = wf
	if bad := timestampSweep(t, db); len(bad) > 0 {
		t.Fatalf("%d stamp(s) are not fixed width:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
}

func TestStamp_auditFilterBoundsUseTheSameFormat(t *testing.T) {
	db, _ := plainDB(t)
	insert := func(id, stamp string) {
		if _, err := db.conn.Exec(`INSERT INTO audit_log (id, entity_type, entity_id, operation, created_at) VALUES (?, 'item', ?, 'created', ?)`, id, id, stamp); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	insert("before", FormatStamp(midnight.Add(-time.Nanosecond)))
	insert("exact", FormatStamp(midnight))
	insert("later", FormatStamp(midnight.Add(500*time.Millisecond)))

	ids := func(f AuditFilter) string {
		rows, err := db.ListAuditLog(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			if r.EntityID.Valid && (r.EntityID.String == "before" || r.EntityID.String == "exact" || r.EntityID.String == "later") {
				out = append(out, r.EntityID.String)
			}
		}
		return strings.Join(out, ",")
	}
	if got := ids(AuditFilter{Today: true}); got != "exact,later" {
		t.Errorf("today = %q (the row at exactly midnight is in, one nanosecond before is out)", got)
	}
	// Since accepts the shapes a person types, and means the same instant in each.
	for _, since := range []string{
		midnight.Format(time.RFC3339),
		midnight.Format(time.RFC3339Nano),
		FormatStamp(midnight),
		midnight.Format("2006-01-02"),
		midnight.In(time.FixedZone("x", -5*3600)).Format(time.RFC3339),
	} {
		if got := ids(AuditFilter{Since: since}); got != "exact,later" {
			t.Errorf("since %q = %q", since, got)
		}
	}
	// A later-than-everything Since returns nothing; a bad one is an error, not a silent mismatch.
	if got := ids(AuditFilter{Since: midnight.Add(time.Second).Format(time.RFC3339)}); got != "" {
		t.Errorf("since after all: %q", got)
	}
	if _, err := db.ListAuditLog(ctx, AuditFilter{Since: "yesterday-ish"}); err == nil {
		t.Error("an unparseable since was accepted")
	}
}

// ---- the one-time rewrite of existing values ----

func TestStamp_normalizeRewritesEveryLegacyShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	db := openAt(t, path)
	p := mustProject(t, db, "demo")
	a := mustItem(t, db, p.ID, "a")
	b := mustItem(t, db, p.ID, "b")
	n, _ := db.AddNote(ctx, AddNoteParams{ItemID: a.ID, Body: "n"})

	// Old writers trimmed trailing zeros; a value could also carry an offset.
	legacy := map[string]string{
		a.ID: "2026-01-01T00:00:00Z",   // whole second: no fraction at all
		b.ID: "2026-01-01T00:00:00.5Z", // trimmed: .500000000
	}
	for id, v := range legacy {
		db.conn.Exec(`UPDATE items SET created_at = ?, updated_at = ? WHERE id = ?`, v, v, id)
	}
	db.conn.Exec(`UPDATE notes SET created_at = '2026-01-01T00:00:00.123Z', updated_at = '2026-01-01T05:00:00+05:00' WHERE id = ?`, n.ID)
	db.conn.Exec(`UPDATE audit_log SET created_at = '2026-01-01T00:00:00.000000001Z' WHERE id = (SELECT id FROM audit_log ORDER BY created_at LIMIT 1)`)
	db.conn.Exec(`UPDATE items SET deleted_at = '2026-01-02T00:00:00.25Z' WHERE id = ?`, b.ID)

	// The bug, shown on real rows: text order and time order disagree.
	var first string
	db.conn.QueryRow(`SELECT id FROM items WHERE id IN (?, ?) ORDER BY created_at LIMIT 1`, a.ID, b.ID).Scan(&first)
	if first != b.ID {
		t.Fatalf("expected the old format to misorder (later row first), got %s", first)
	}
	before := snapshotStamps(t, db)
	nonNull := countNonNull(t, db)
	if len(timestampSweep(t, db)) == 0 {
		t.Fatal("test setup: no legacy values")
	}

	// A dry run reports and changes nothing.
	rep, err := db.NormalizeTimestamps(ctx, false)
	if err != nil || rep.Applied || rep.Rewritten == 0 || len(rep.Unparseable) != 0 {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	if !reflect.DeepEqual(snapshotStamps(t, db), before) {
		t.Fatal("a dry run changed data")
	}

	rep, err = db.NormalizeTimestamps(ctx, true)
	if err != nil || !rep.Applied || rep.Rewritten == 0 {
		t.Fatalf("apply: %+v %v", rep, err)
	}
	if bad := timestampSweep(t, db); len(bad) != 0 {
		t.Errorf("still not fixed width: %v", bad)
	}
	// Same instants: every rewritten value names the moment it named before.
	after := snapshotStamps(t, db)
	for key, old := range before {
		o, err1 := time.Parse(time.RFC3339Nano, old)
		nw, err2 := time.Parse(time.RFC3339Nano, after[key])
		if err1 != nil || err2 != nil || !o.Equal(nw) {
			t.Errorf("%s: %q became %q", key, old, after[key])
		}
	}
	// Nothing appeared or vanished, and a NULL stayed NULL.
	if !reflect.DeepEqual(countNonNull(t, db), nonNull) {
		t.Errorf("non-null counts changed: %v -> %v", nonNull, countNonNull(t, db))
	}
	var del sql.NullString
	db.conn.QueryRow(`SELECT deleted_at FROM items WHERE id = ?`, a.ID).Scan(&del)
	if del.Valid {
		t.Error("a NULL deleted_at became a value")
	}
	// The bug is gone: text order is time order.
	db.conn.QueryRow(`SELECT id FROM items WHERE id IN (?, ?) ORDER BY created_at LIMIT 1`, a.ID, b.ID).Scan(&first)
	if first != a.ID {
		t.Errorf("after the rewrite the earlier row should sort first, got %s", first)
	}
	// An offset value was written as the same instant in UTC.
	var upd string
	db.conn.QueryRow(`SELECT updated_at FROM notes WHERE id = ?`, n.ID).Scan(&upd)
	if upd != "2026-01-01T00:00:00.000000000Z" {
		t.Errorf("offset value: %q", upd)
	}
	// Idempotent: a second run has nothing to do.
	rep, err = db.NormalizeTimestamps(ctx, true)
	if err != nil || rep.Rewritten != 0 {
		t.Errorf("second run: %+v %v", rep, err)
	}
}

func TestStamp_normalizeRefusesWhenAValueCannotBeRead(t *testing.T) {
	db := openTest(t)
	p := mustProject(t, db, "demo")
	a := mustItem(t, db, p.ID, "a")
	b := mustItem(t, db, p.ID, "b")
	db.conn.Exec(`UPDATE items SET created_at = '2026-01-01T00:00:00Z' WHERE id = ?`, a.ID)
	db.conn.Exec(`UPDATE items SET created_at = 'last tuesday' WHERE id = ?`, b.ID)
	before := snapshotStamps(t, db)

	rep, err := db.NormalizeTimestamps(ctx, true)
	if err == nil || rep == nil || len(rep.Unparseable) != 1 || !strings.Contains(rep.Unparseable[0], "last tuesday") {
		t.Fatalf("it must refuse and name the value: %+v %v", rep, err)
	}
	if rep.Applied {
		t.Error("it reports having applied")
	}
	if !reflect.DeepEqual(snapshotStamps(t, db), before) {
		t.Error("a refused rewrite changed other rows: it must be all or nothing")
	}
}

func TestStamp_normalizeOnAFreshLedgerIsANoOp(t *testing.T) {
	db := openAs(t, filepath.Join(t.TempDir(), "l.db"), alice, nil)
	p := mustProject(t, db, "demo")
	mustItem(t, db, p.ID, "a")
	rep, err := db.NormalizeTimestamps(ctx, true)
	if err != nil || rep.Rewritten != 0 || len(rep.Columns) == 0 {
		t.Errorf("%+v %v", rep, err)
	}
	for _, c := range rep.Columns {
		if c.Rewritten != 0 || c.Fixed != c.NonNull {
			t.Errorf("%s.%s: %+v", c.Table, c.Column, c)
		}
	}
}

// snapshotStamps maps "table.column.key" to its value for every *_at value, keyed by
// each table's primary key.
func snapshotStamps(t *testing.T, db *DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	cols, err := timestampColumns(ctx, db.conn.d, db.conn)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		rows, err := db.conn.Query(fmt.Sprintf(`SELECT %s, %s FROM %s WHERE %s IS NOT NULL`, c.pk, c.column, c.table, c.column))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var key, v string
			if err := rows.Scan(&key, &v); err != nil {
				t.Fatal(err)
			}
			out[c.table+"."+c.column+"."+key] = v
		}
		rows.Close()
	}
	return out
}

// countNonNull counts non-null values per table.column.
func countNonNull(t *testing.T, db *DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for k := range snapshotStamps(t, db) {
		parts := strings.Split(k, ".")
		out[parts[0]+"."+parts[1]]++
	}
	return out
}
