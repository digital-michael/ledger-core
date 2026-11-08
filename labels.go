package ledgercore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Labels: zero or more free-form tags per item (ledger item 20a5d442).
//
// item_labels replaces the single items.label column. The rules, decided with
// the operator 2026-11-07:
//
//   - no label is ever lost: every existing items.label value is copied across
//     (migrateItemLabels) before anything is dropped;
//   - labels are unique per item, compared case-insensitively and trimmed, and
//     the database enforces it (a partial unique index over live rows), so a
//     removed label can be added again;
//   - the old column is left untouched as the safety copy. Dropping it is a
//     separate, gated step (LabelMigrationReport).
//
// The label vocabulary itself stays free-form: no pool, no validation beyond
// shape. That is the whole difference from the constrained component field.

// maxLabelLen bounds a label so a pasted paragraph is not stored as a tag.
const maxLabelLen = 100

// ItemLabel is one live label on one item.
type ItemLabel struct {
	ID        string
	ItemID    string
	Label     string
	CreatedAt string
	CreatedBy sql.NullString // actor "kind:id"; NULL for labels copied from the old column
}

// LabelCount is a distinct label and how many live items carry it.
type LabelCount struct {
	Label string
	Count int
}

// normalizeLabel trims and validates one label. The stored spelling is the
// trimmed one; comparison everywhere is case-insensitive.
func normalizeLabel(s string) (string, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return "", errors.New("a label cannot be empty")
	}
	if utf8.RuneCountInString(v) > maxLabelLen {
		return "", fmt.Errorf("a label is at most %d characters", maxLabelLen)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", errors.New("a label cannot contain line breaks, tabs or other control characters")
		}
	}
	return v, nil
}

// normalizeLabels normalises each and drops case-insensitive duplicates,
// keeping the first spelling.
func normalizeLabels(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		v, err := normalizeLabel(s)
		if err != nil {
			return nil, err
		}
		k := strings.ToLower(v)
		if !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	return out, nil
}

// sortLabels orders labels case-insensitively, then by spelling.
func sortLabels(ls []string) {
	sort.Slice(ls, func(i, j int) bool {
		a, b := strings.ToLower(ls[i]), strings.ToLower(ls[j])
		if a != b {
			return a < b
		}
		return ls[i] < ls[j]
	})
}

// labelsSelect is the column that lets every item query return its labels in
// the same row, so no reader needs a second query. Unit-separator-joined;
// control characters are refused in labels, so it cannot collide.
const labelsSelect = `(SELECT group_concat(label, char(31)) FROM item_labels WHERE item_id = items.id AND deleted_at IS NULL)`

// setItemLabels fills it.Labels and the legacy it.Label from the joined column.
// Label stays as a compatibility view -- all labels, ", "-joined, NULL when
// there are none -- so consumers that still read one string keep working until
// they move to Labels.
func setItemLabels(it *Item, joined sql.NullString) {
	it.Labels = nil
	it.Label = sql.NullString{}
	if !joined.Valid || joined.String == "" {
		return
	}
	it.Labels = strings.Split(joined.String, "\x1f")
	sortLabels(it.Labels)
	it.Label = sql.NullString{String: strings.Join(it.Labels, ", "), Valid: true}
}

// liveLabels reads an item's live labels inside tx, in order.
func liveLabels(ctx context.Context, tx *sql.Tx, itemID string) ([]ItemLabel, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, item_id, label, created_at, created_by FROM item_labels
		 WHERE item_id = ? AND deleted_at IS NULL ORDER BY lower(label), label`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemLabel
	for rows.Next() {
		var l ItemLabel
		if err := rows.Scan(&l.ID, &l.ItemID, &l.Label, &l.CreatedAt, &l.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// touchItem moves an item's updated_at, so a conflict check made against the
// version someone loaded notices that its labels changed.
func touchItem(ctx context.Context, tx *sql.Tx, itemID, now string) error {
	_, err := tx.ExecContext(ctx, `UPDATE items SET updated_at = ? WHERE id = ?`, now, itemID)
	return err
}

// addLabelTx adds label to the item unless it already has it (any case).
// added is false for the no-op. The item must already be checked live.
func (db *DB) addLabelTx(ctx context.Context, tx *sql.Tx, itemID, label, now string) (*ItemLabel, bool, error) {
	var ex ItemLabel
	err := tx.QueryRowContext(ctx,
		`SELECT id, item_id, label, created_at, created_by FROM item_labels
		 WHERE item_id = ? AND lower(label) = lower(?) AND deleted_at IS NULL`, itemID, label).
		Scan(&ex.ID, &ex.ItemID, &ex.Label, &ex.CreatedAt, &ex.CreatedBy)
	if err == nil {
		return &ex, false, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}
	by := nullIfEmpty(db.currentActor(ctx).String())
	l := ItemLabel{ID: uuid.NewString(), ItemID: itemID, Label: label, CreatedAt: now}
	if by != nil {
		l.CreatedBy = sql.NullString{String: by.(string), Valid: true}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO item_labels (id, item_id, label, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		l.ID, itemID, label, now, by); err != nil {
		return nil, false, fmt.Errorf("adding label: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"item_id": itemID, "label": label})
	if err := db.insertAudit(ctx, tx, "item_label", l.ID, "created", string(detail)); err != nil {
		return nil, false, fmt.Errorf("writing audit log: %w", err)
	}
	return &l, true, nil
}

// removeLabelRowTx soft-deletes one label row, with its audit entry.
func (db *DB) removeLabelRowTx(ctx context.Context, tx *sql.Tx, l ItemLabel, now string, extra map[string]string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE item_labels SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, now, l.ID); err != nil {
		return fmt.Errorf("removing label: %w", err)
	}
	d := map[string]string{"item_id": l.ItemID, "label": l.Label}
	for k, v := range extra {
		d[k] = v
	}
	detail, _ := json.Marshal(d)
	if err := db.insertAudit(ctx, tx, "item_label", l.ID, "deleted", string(detail)); err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	return nil
}

// AddLabel adds a label to an item. Adding one the item already has (any
// case, any surrounding space) changes nothing and returns the existing label
// with added=false. A deleted item refuses.
func (db *DB) AddLabel(ctx context.Context, itemID, label string) (*ItemLabel, bool, error) {
	v, err := normalizeLabel(label)
	if err != nil {
		return nil, false, err
	}
	id, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return nil, false, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := refuseIfItemDeleted(ctx, tx, id); err != nil {
		return nil, false, err
	}
	now := nowUTC()
	l, added, err := db.addLabelTx(ctx, tx, id, v, now)
	if err != nil {
		return nil, false, err
	}
	if added {
		if err := touchItem(ctx, tx, id, now); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return l, added, nil
}

// RemoveLabel removes a label from an item by value (any case). removed is
// false if the item did not have it. It is a soft delete: the row and its
// audit trail remain, and the label can be added again.
func (db *DB) RemoveLabel(ctx context.Context, itemID, label string) (bool, error) {
	v, err := normalizeLabel(label)
	if err != nil {
		return false, err
	}
	id, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return false, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := refuseIfItemDeleted(ctx, tx, id); err != nil {
		return false, err
	}
	var l ItemLabel
	err = tx.QueryRowContext(ctx,
		`SELECT id, item_id, label, created_at, created_by FROM item_labels
		 WHERE item_id = ? AND lower(label) = lower(?) AND deleted_at IS NULL`, id, v).
		Scan(&l.ID, &l.ItemID, &l.Label, &l.CreatedAt, &l.CreatedBy)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := nowUTC()
	if err := db.removeLabelRowTx(ctx, tx, l, now, nil); err != nil {
		return false, err
	}
	if err := touchItem(ctx, tx, id, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListLabels returns an item's live labels, in order.
func (db *DB) ListLabels(ctx context.Context, itemID string) ([]ItemLabel, error) {
	id, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return nil, err
	}
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, item_id, label, created_at, created_by FROM item_labels
		 WHERE item_id = ? AND deleted_at IS NULL ORDER BY lower(label), label`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemLabel
	for rows.Next() {
		var l ItemLabel
		if err := rows.Scan(&l.ID, &l.ItemID, &l.Label, &l.CreatedAt, &l.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListDistinctLabels returns every label in use on live items with how many
// items carry it, sorted. Case variants count as one label, shown in their
// first-sorting spelling. projectIDs narrows it; empty means every project.
func (db *DB) ListDistinctLabels(ctx context.Context, projectIDs []string) ([]LabelCount, error) {
	q := `SELECT MIN(l.label), COUNT(DISTINCT l.item_id)
		FROM item_labels l JOIN items i ON i.id = l.item_id
		WHERE l.deleted_at IS NULL AND i.deleted_at IS NULL`
	var args []any
	if len(projectIDs) > 0 {
		q += ` AND i.project_id IN (?` + strings.Repeat(`, ?`, len(projectIDs)-1) + `)`
		for _, p := range projectIDs {
			args = append(args, p)
		}
	}
	q += ` GROUP BY lower(l.label) ORDER BY lower(l.label)`
	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LabelCount
	for rows.Next() {
		var c LabelCount
		if err := rows.Scan(&c.Label, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReplaceLabels renames a label value on every live item that holds it, in one
// transaction and one audit batch. An item that already has the new label just
// loses the old one, so no item ends up with a duplicate. It returns the
// number of items changed.
func (db *DB) ReplaceLabels(ctx context.Context, existing, newLabel string) (int, error) {
	old, err := normalizeLabel(existing)
	if err != nil {
		return 0, fmt.Errorf("existing label: %w", err)
	}
	nw, err := normalizeLabel(newLabel)
	if err != nil {
		return 0, fmt.Errorf("new label: %w", err)
	}
	if err := db.permit(ctx, OpBulkReplaceLabels, Target{EntityType: "item_label"}); err != nil {
		return 0, err
	}
	ctx = WithBatch(ctx, uuid.NewString())

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT l.id, l.item_id, l.label, l.created_at, l.created_by
		 FROM item_labels l JOIN items i ON i.id = l.item_id
		 WHERE lower(l.label) = lower(?) AND l.deleted_at IS NULL AND i.deleted_at IS NULL
		 ORDER BY l.item_id`, old)
	if err != nil {
		return 0, err
	}
	var targets []ItemLabel
	for rows.Next() {
		var l ItemLabel
		if err := rows.Scan(&l.ID, &l.ItemID, &l.Label, &l.CreatedAt, &l.CreatedBy); err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	now := nowUTC()
	changed := 0
	for _, l := range targets {
		var haveID string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM item_labels WHERE item_id = ? AND lower(label) = lower(?) AND deleted_at IS NULL AND id <> ?`,
			l.ItemID, nw, l.ID).Scan(&haveID)
		switch {
		case err == nil:
			// The item already has the new label: drop the old row, no duplicate.
			if err := db.removeLabelRowTx(ctx, tx, l, now, map[string]string{"replaced_by": nw}); err != nil {
				return 0, err
			}
		case err == sql.ErrNoRows:
			if l.Label == nw {
				continue // already exactly this spelling
			}
			if _, err := tx.ExecContext(ctx, `UPDATE item_labels SET label = ? WHERE id = ?`, nw, l.ID); err != nil {
				return 0, fmt.Errorf("renaming label: %w", err)
			}
			detail, _ := json.Marshal(map[string]string{"item_id": l.ItemID, "from": l.Label, "to": nw})
			if err := db.insertAudit(ctx, tx, "item_label", l.ID, "updated", string(detail)); err != nil {
				return 0, fmt.Errorf("writing audit log: %w", err)
			}
		default:
			return 0, err
		}
		if err := touchItem(ctx, tx, l.ItemID, now); err != nil {
			return 0, err
		}
		changed++
	}
	return changed, tx.Commit()
}

// replaceItemLabelsTx makes the item's live labels exactly want (already
// normalised and de-duplicated): removes the ones not wanted, adds the ones
// missing. It reports whether anything changed and the before/after text.
func (db *DB) replaceItemLabelsTx(ctx context.Context, tx *sql.Tx, itemID string, want []string, now string) (changed bool, before, after string, err error) {
	cur, err := liveLabels(ctx, tx, itemID)
	if err != nil {
		return false, "", "", err
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[strings.ToLower(w)] = true
	}
	haveSet := map[string]bool{}
	var beforeList []string
	for _, l := range cur {
		haveSet[strings.ToLower(l.Label)] = true
		beforeList = append(beforeList, l.Label)
	}
	for _, l := range cur {
		if !wantSet[strings.ToLower(l.Label)] {
			if err := db.removeLabelRowTx(ctx, tx, l, now, nil); err != nil {
				return false, "", "", err
			}
			changed = true
		}
	}
	for _, w := range want {
		if !haveSet[strings.ToLower(w)] {
			if _, _, err := db.addLabelTx(ctx, tx, itemID, w, now); err != nil {
				return false, "", "", err
			}
			changed = true
		}
	}
	afterList := append([]string(nil), want...)
	sortLabels(afterList)
	return changed, strings.Join(beforeList, ", "), strings.Join(afterList, ", "), nil
}

// migrateItemLabels copies every legacy items.label value into item_labels,
// once. An item is copied only if it has no item_labels row at all -- live or
// removed -- so a label removed after the copy is never resurrected by a later
// open, and the copy is safe to run on every open. items.label is not touched.
// It returns how many labels were copied.
func migrateItemLabels(ctx context.Context, conn *sql.DB) (int, error) {
	if has, err := hasColumn(conn, "items", "label"); err != nil || !has {
		return 0, err // nothing to copy: the column is gone (dropped, or a new database)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		`SELECT id, label, created_at FROM items
		 WHERE label IS NOT NULL AND TRIM(label) <> ''
		   AND NOT EXISTS (SELECT 1 FROM item_labels l WHERE l.item_id = items.id)`)
	if err != nil {
		return 0, err
	}
	type legacy struct{ id, label, createdAt string }
	var todo []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.id, &l.label, &l.createdAt); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, l := range todo {
		v := strings.TrimSpace(l.label)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO item_labels (id, item_id, label, created_at) VALUES (?, ?, ?, ?)`,
			uuid.NewString(), l.id, v, l.createdAt); err != nil {
			return 0, fmt.Errorf("copying label of item %s: %w", l.id, err)
		}
	}
	return len(todo), tx.Commit()
}

// LabelGateProject is one project's line in the zero-loss check.
type LabelGateProject struct {
	Project string
	Legacy  int // items with a non-empty legacy items.label
	Copied  int // of those, items that have at least one item_labels row (live or removed)
}

// LabelGateReport is the check that must pass before items.label is dropped:
// every item that has a legacy label must be represented in item_labels.
type LabelGateReport struct {
	Projects []LabelGateProject // projects with at least one legacy label
	Legacy   int
	Copied   int
}

// OK is true when no legacy label is missing from item_labels.
func (r LabelGateReport) OK() bool {
	for _, p := range r.Projects {
		if p.Copied != p.Legacy {
			return false
		}
	}
	return true
}

// LabelMigrationReport counts, per project, legacy labels against the items
// that carry them in item_labels. It only reads.
func (db *DB) LabelMigrationReport(ctx context.Context) (*LabelGateReport, error) {
	if has, err := hasColumn(db.conn, "items", "label"); err != nil {
		return nil, err
	} else if !has {
		return &LabelGateReport{}, nil // no legacy column, so no legacy label can be missing
	}
	rows, err := db.conn.QueryContext(ctx,
		`SELECT p.key, COUNT(*),
		        SUM(CASE WHEN EXISTS (SELECT 1 FROM item_labels l WHERE l.item_id = i.id) THEN 1 ELSE 0 END)
		 FROM items i JOIN projects p ON p.id = i.project_id
		 WHERE i.label IS NOT NULL AND TRIM(i.label) <> ''
		 GROUP BY p.key ORDER BY p.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rep := &LabelGateReport{}
	for rows.Next() {
		var p LabelGateProject
		if err := rows.Scan(&p.Project, &p.Legacy, &p.Copied); err != nil {
			return nil, err
		}
		rep.Projects = append(rep.Projects, p)
		rep.Legacy += p.Legacy
		rep.Copied += p.Copied
	}
	return rep, rows.Err()
}

// DropLegacyLabelColumn removes items.label, the column item_labels replaced.
// It is the one irreversible step of the label migration, so it is gated: it
// refuses unless LabelMigrationReport passes, meaning every item that has a
// legacy label is represented in item_labels. dropped is false when the column
// was already gone, so running it twice is harmless.
//
// Nothing may be reading the column when it goes: an older program that still
// selects items.label fails loudly afterwards ("no such column"), which is why
// every program that opens the ledger must be on the release that uses
// item_labels first. Take a backup before calling it.
func (db *DB) DropLegacyLabelColumn(ctx context.Context) (dropped bool, err error) {
	has, err := hasColumn(db.conn, "items", "label")
	if err != nil || !has {
		return false, err
	}
	rep, err := db.LabelMigrationReport(ctx)
	if err != nil {
		return false, err
	}
	if !rep.OK() {
		var bad []string
		for _, p := range rep.Projects {
			if p.Copied != p.Legacy {
				bad = append(bad, fmt.Sprintf("%s: %d legacy labels, %d in item_labels", p.Project, p.Legacy, p.Copied))
			}
		}
		return false, fmt.Errorf("refusing to drop items.label: the zero-loss check failed (%s)", strings.Join(bad, "; "))
	}
	if err := ensureColumnDropped(db.conn, "items", "label"); err != nil {
		return false, err
	}
	return true, nil
}
