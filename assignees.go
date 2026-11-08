package ledgercore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Assignees and watchers (decided 2026-11-07).
//
// Three child tables of one shape -- item_assignees, item_watchers,
// project_watchers -- the same pattern as labels: a row per member, unique per
// owner among live rows (a partial unique index), soft delete, every change
// audited. A member is an identity "kind:id" (identity.go); a person or an agent
// may be an assignee or a watcher.
//
// The meanings: the owner is accountable, the assignees are doing the work now,
// the watchers are informed. Nothing here delivers a notification; that is a
// separate ticket.
//
// The single items.assignee column they replace is copied once (migrateItemAssignees)
// and dropped only by a separate, gated step (DropLegacyAssigneeColumn).

// peopleTable describes one of the three tables.
type peopleTable struct {
	table    string        // item_assignees
	ownerCol string        // item_id | project_id
	entity   string        // the audit entity type, and the deletable entity type
	field    IdentityField // which kinds may be members
}

var (
	assigneeTable       = peopleTable{"item_assignees", "item_id", "item_assignee", FieldAssignee}
	watcherTable        = peopleTable{"item_watchers", "item_id", "item_watcher", FieldWatcher}
	projectWatcherTable = peopleTable{"project_watchers", "project_id", "project_watcher", FieldWatcher}
)

// Participant is one live assignee or watcher.
type Participant struct {
	ID        string
	OwnerID   string // the item or project it belongs to
	Member    string // "kind:id"
	CreatedAt string
	CreatedBy sql.NullString
}

// Column selects that let every item query return its people in the same row.
// Unit-separator-joined; an identity cannot contain a control character.
const (
	assigneesSelect = `(SELECT group_concat(member, char(31)) FROM item_assignees WHERE item_id = items.id AND deleted_at IS NULL)`
	watchersSelect  = `(SELECT group_concat(member, char(31)) FROM item_watchers WHERE item_id = items.id AND deleted_at IS NULL)`
)

// splitPeople turns a joined column into a sorted list.
func splitPeople(joined sql.NullString) []string {
	if !joined.Valid || joined.String == "" {
		return nil
	}
	out := strings.Split(joined.String, "\x1f")
	sort.Strings(out)
	return out
}

// normalizeIdentities checks each as the field's identity and drops duplicates.
func normalizeIdentities(field IdentityField, in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		id, err := ParseIdentity(field, s)
		if err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

func scanParticipant(row interface{ Scan(...any) error }) (*Participant, error) {
	var p Participant
	if err := row.Scan(&p.ID, &p.OwnerID, &p.Member, &p.CreatedAt, &p.CreatedBy); err != nil {
		return nil, err
	}
	return &p, nil
}

func (t peopleTable) columns() string {
	return "id, " + t.ownerCol + ", member, created_at, created_by"
}

// addPersonTx adds member (already normalised) unless it is already a live member.
func (db *DB) addPersonTx(ctx context.Context, tx *sql.Tx, t peopleTable, ownerID, member, now string) (*Participant, bool, error) {
	ex, err := scanParticipant(tx.QueryRowContext(ctx,
		`SELECT `+t.columns()+` FROM `+t.table+` WHERE `+t.ownerCol+` = ? AND member = ? AND deleted_at IS NULL`, ownerID, member))
	if err == nil {
		return ex, false, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}
	by, byNull := db.creatorColumn(ctx)
	p := &Participant{ID: uuid.NewString(), OwnerID: ownerID, Member: member, CreatedAt: now, CreatedBy: byNull}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+t.table+` (id, `+t.ownerCol+`, member, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		p.ID, ownerID, member, now, by); err != nil {
		return nil, false, fmt.Errorf("adding %s: %w", t.entity, err)
	}
	detail, _ := json.Marshal(map[string]string{t.ownerCol: ownerID, "member": member})
	if err := db.insertAudit(ctx, tx, t.entity, p.ID, "created", string(detail)); err != nil {
		return nil, false, fmt.Errorf("writing audit log: %w", err)
	}
	return p, true, nil
}

// removePersonTx soft-deletes member's live row, if any.
func (db *DB) removePersonTx(ctx context.Context, tx *sql.Tx, t peopleTable, ownerID, member, now string) (bool, error) {
	p, err := scanParticipant(tx.QueryRowContext(ctx,
		`SELECT `+t.columns()+` FROM `+t.table+` WHERE `+t.ownerCol+` = ? AND member = ? AND deleted_at IS NULL`, ownerID, member))
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+t.table+` SET deleted_at = ? WHERE id = ?`, now, p.ID); err != nil {
		return false, fmt.Errorf("removing %s: %w", t.entity, err)
	}
	detail, _ := json.Marshal(map[string]string{t.ownerCol: ownerID, "member": member})
	if err := db.insertAudit(ctx, tx, t.entity, p.ID, "deleted", string(detail)); err != nil {
		return false, fmt.Errorf("writing audit log: %w", err)
	}
	return true, nil
}

func (db *DB) listPeople(ctx context.Context, t peopleTable, ownerID string) ([]Participant, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT `+t.columns()+` FROM `+t.table+` WHERE `+t.ownerCol+` = ? AND deleted_at IS NULL ORDER BY lower(member)`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Participant
	for rows.Next() {
		p, err := scanParticipant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// livePeopleTx lists an item's live members in order, inside tx.
func livePeopleTx(ctx context.Context, tx *sql.Tx, t peopleTable, ownerID string) ([]Participant, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+t.columns()+` FROM `+t.table+` WHERE `+t.ownerCol+` = ? AND deleted_at IS NULL ORDER BY lower(member)`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Participant
	for rows.Next() {
		p, err := scanParticipant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func joinMembers(ps []Participant) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Member
	}
	return strings.Join(names, ", ")
}

// replacePeopleTx makes the item's live members of one table exactly want
// (normalised, unique): removes the ones not wanted, adds the missing. Returns
// whether anything changed and the before/after text.
func (db *DB) replacePeopleTx(ctx context.Context, tx *sql.Tx, t peopleTable, itemID string, want []string, now string) (changed bool, before, after string, err error) {
	cur, err := livePeopleTx(ctx, tx, t, itemID)
	if err != nil {
		return false, "", "", err
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	have := map[string]bool{}
	for _, p := range cur {
		have[p.Member] = true
		if !wantSet[p.Member] {
			if _, err := db.removePersonTx(ctx, tx, t, itemID, p.Member, now); err != nil {
				return false, "", "", err
			}
			changed = true
		}
	}
	for _, w := range want {
		if !have[w] {
			if _, _, err := db.addPersonTx(ctx, tx, t, itemID, w, now); err != nil {
				return false, "", "", err
			}
			changed = true
		}
	}
	after = strings.Join(want, ", ")
	return changed, joinMembers(cur), after, nil
}

// itemPeopleTx runs fn on a live item inside a transaction: resolves the id,
// refuses a deleted item, and commits.
func (db *DB) itemPeopleTx(ctx context.Context, itemID string, fn func(tx *sql.Tx, id string) error) error {
	id, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := refuseIfItemDeleted(ctx, tx, id); err != nil {
		return err
	}
	if err := fn(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// auditAssigned records, on the item itself, that its assignees changed -- the
// "assigned" operation the item's history has always shown.
func (db *DB) auditAssigned(ctx context.Context, tx *sql.Tx, itemID, before, after string) error {
	detail, _ := json.Marshal(map[string]string{"from": before, "to": after})
	return db.insertAudit(ctx, tx, "item", itemID, "assigned", string(detail))
}

// AddAssignee makes a person or agent an assignee of the item. Adding one who is
// already assigned changes nothing (added=false).
func (db *DB) AddAssignee(ctx context.Context, itemID, member string) (*Participant, bool, error) {
	who, err := ParseIdentity(FieldAssignee, member)
	if err != nil {
		return nil, false, err
	}
	var p *Participant
	var added bool
	err = db.itemPeopleTx(ctx, itemID, func(tx *sql.Tx, id string) error {
		before, err := livePeopleTx(ctx, tx, assigneeTable, id)
		if err != nil {
			return err
		}
		now := nowUTC()
		if p, added, err = db.addPersonTx(ctx, tx, assigneeTable, id, who, now); err != nil || !added {
			return err
		}
		after := joinMembers(append(before, *p))
		if err := touchItem(ctx, tx, id, now); err != nil {
			return err
		}
		return db.auditAssigned(ctx, tx, id, joinMembers(before), sortedJoin(after))
	})
	return p, added && err == nil, err
}

// sortedJoin re-sorts a ", "-joined list, so before/after read in one order.
func sortedJoin(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(s, ", ")
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// RemoveAssignee removes an assignee. removed is false if they were not assigned.
func (db *DB) RemoveAssignee(ctx context.Context, itemID, member string) (bool, error) {
	who, err := ParseIdentity(FieldAssignee, member)
	if err != nil {
		return false, err
	}
	removed := false
	err = db.itemPeopleTx(ctx, itemID, func(tx *sql.Tx, id string) error {
		before, err := livePeopleTx(ctx, tx, assigneeTable, id)
		if err != nil {
			return err
		}
		now := nowUTC()
		if removed, err = db.removePersonTx(ctx, tx, assigneeTable, id, who, now); err != nil || !removed {
			return err
		}
		after, err := livePeopleTx(ctx, tx, assigneeTable, id)
		if err != nil {
			return err
		}
		if err := touchItem(ctx, tx, id, now); err != nil {
			return err
		}
		return db.auditAssigned(ctx, tx, id, joinMembers(before), joinMembers(after))
	})
	return removed && err == nil, err
}

// ListAssignees returns the item's live assignees, by name.
func (db *DB) ListAssignees(ctx context.Context, itemID string) ([]Participant, error) {
	id, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return nil, err
	}
	return db.listPeople(ctx, assigneeTable, id)
}

// AddWatcher makes a person or agent a watcher of the item.
func (db *DB) AddWatcher(ctx context.Context, itemID, member string) (*Participant, bool, error) {
	who, err := ParseIdentity(FieldWatcher, member)
	if err != nil {
		return nil, false, err
	}
	var p *Participant
	var added bool
	err = db.itemPeopleTx(ctx, itemID, func(tx *sql.Tx, id string) error {
		now := nowUTC()
		var err error
		if p, added, err = db.addPersonTx(ctx, tx, watcherTable, id, who, now); err != nil || !added {
			return err
		}
		return touchItem(ctx, tx, id, now)
	})
	return p, added && err == nil, err
}

// RemoveWatcher removes a watcher of the item.
func (db *DB) RemoveWatcher(ctx context.Context, itemID, member string) (bool, error) {
	who, err := ParseIdentity(FieldWatcher, member)
	if err != nil {
		return false, err
	}
	removed := false
	err = db.itemPeopleTx(ctx, itemID, func(tx *sql.Tx, id string) error {
		now := nowUTC()
		var err error
		if removed, err = db.removePersonTx(ctx, tx, watcherTable, id, who, now); err != nil || !removed {
			return err
		}
		return touchItem(ctx, tx, id, now)
	})
	return removed && err == nil, err
}

// ListWatchers returns the item's live watchers.
func (db *DB) ListWatchers(ctx context.Context, itemID string) ([]Participant, error) {
	id, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return nil, err
	}
	return db.listPeople(ctx, watcherTable, id)
}

// projectPeopleTx is itemPeopleTx for a project.
func (db *DB) projectPeopleTx(ctx context.Context, projectID string, fn func(tx *sql.Tx) error) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := refuseIfProjectDeleted(ctx, tx, projectID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// AddProjectWatcher makes a person or agent a watcher of a whole project.
func (db *DB) AddProjectWatcher(ctx context.Context, projectID, member string) (*Participant, bool, error) {
	who, err := ParseIdentity(FieldWatcher, member)
	if err != nil {
		return nil, false, err
	}
	var p *Participant
	var added bool
	err = db.projectPeopleTx(ctx, projectID, func(tx *sql.Tx) error {
		var err error
		p, added, err = db.addPersonTx(ctx, tx, projectWatcherTable, projectID, who, nowUTC())
		return err
	})
	return p, added && err == nil, err
}

// RemoveProjectWatcher removes a project watcher.
func (db *DB) RemoveProjectWatcher(ctx context.Context, projectID, member string) (bool, error) {
	who, err := ParseIdentity(FieldWatcher, member)
	if err != nil {
		return false, err
	}
	removed := false
	err = db.projectPeopleTx(ctx, projectID, func(tx *sql.Tx) error {
		var err error
		removed, err = db.removePersonTx(ctx, tx, projectWatcherTable, projectID, who, nowUTC())
		return err
	})
	return removed && err == nil, err
}

// ListProjectWatchers returns a project's live watchers.
func (db *DB) ListProjectWatchers(ctx context.Context, projectID string) ([]Participant, error) {
	return db.listPeople(ctx, projectWatcherTable, projectID)
}

// ---- defaults at creation ----

// createPeopleTx adds a new item's assignees and watchers. Explicit lists win
// (a non-nil empty list means none). Otherwise, for a ticket an AGENT creates:
// it is assigned to that agent, and the project's owners watch it. That is the
// operator's working convention (2026-11-07) and it is per agent: another agent
// is assigned to its own work. A person's or a program's ticket gets no default.
func (db *DB) createPeopleTx(ctx context.Context, tx *sql.Tx, itemID, projectID string, p CreateItemParams, creator Actor, now string) error {
	var assignees, watchers []string
	var err error
	switch {
	case p.Assignees != nil:
		if assignees, err = normalizeIdentities(FieldAssignee, p.Assignees); err != nil {
			return err
		}
	case p.Assignee != "":
		if assignees, err = normalizeIdentities(FieldAssignee, []string{p.Assignee}); err != nil {
			return err
		}
	case creator.Kind == ActorAgent:
		if who, err := ParseIdentity(FieldAssignee, creator.String()); err == nil {
			assignees = []string{who}
		}
	}
	if p.Watchers != nil {
		if watchers, err = normalizeIdentities(FieldWatcher, p.Watchers); err != nil {
			return err
		}
	} else if creator.Kind == ActorAgent {
		rows, err := tx.QueryContext(ctx,
			`SELECT member FROM project_members WHERE project_id = ? AND role = 'owner' AND deleted_at IS NULL ORDER BY lower(member)`, projectID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				rows.Close()
				return err
			}
			watchers = append(watchers, m)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	for _, a := range assignees {
		if _, _, err := db.addPersonTx(ctx, tx, assigneeTable, itemID, a, now); err != nil {
			return err
		}
	}
	for _, w := range watchers {
		if _, _, err := db.addPersonTx(ctx, tx, watcherTable, itemID, w, now); err != nil {
			return err
		}
	}
	return nil
}

// ---- migration of the legacy items.assignee column ----

// migrateItemAssignees copies each legacy items.assignee value into
// item_assignees, once. The old values were free text; the two spellings in use
// ("Michael", "digital-michael") are the workstation person (LegacyIdentity), and
// a value already in the kind:id form is kept. Anything else is NOT guessed: it
// is left where it is and reported by AssigneeMigrationReport, which blocks the
// drop until a person decides. An item is copied only if it has no item_assignees
// row at all, so a removal is never undone by a later open.
func migrateItemAssignees(ctx context.Context, conn *sql.DB) (int, error) {
	if has, err := hasColumn(conn, "items", "assignee"); err != nil || !has {
		return 0, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		`SELECT id, assignee, created_at FROM items
		 WHERE assignee IS NOT NULL AND TRIM(assignee) <> ''
		   AND NOT EXISTS (SELECT 1 FROM item_assignees a WHERE a.item_id = items.id)`)
	if err != nil {
		return 0, err
	}
	type legacy struct{ id, value, createdAt string }
	var todo []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.id, &l.value, &l.createdAt); err != nil {
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
	copied := 0
	for _, l := range todo {
		who, ok := resolveLegacyAssignee(l.value)
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO item_assignees (id, item_id, member, created_at) VALUES (?, ?, ?, ?)`,
			uuid.NewString(), l.id, who, l.createdAt); err != nil {
			return 0, fmt.Errorf("copying assignee of item %s: %w", l.id, err)
		}
		copied++
	}
	return copied, tx.Commit()
}

// resolveLegacyAssignee turns an old free-text assignee into an identity.
func resolveLegacyAssignee(v string) (string, bool) {
	if id, ok := LegacyIdentity(v); ok {
		return id, true
	}
	if id, err := ParseIdentity(FieldAssignee, v); err == nil {
		return id, true
	}
	return "", false
}

// AssigneeGateProject is one project's line in the zero-loss check.
type AssigneeGateProject struct {
	Project string
	Legacy  int // items with a non-empty legacy items.assignee
	Copied  int // of those, items that have at least one item_assignees row
}

// AssigneeGateReport is the check that must pass before items.assignee is dropped.
type AssigneeGateReport struct {
	Projects   []AssigneeGateProject
	Legacy     int
	Copied     int
	Unresolved []string // legacy values nobody mapped to an identity, and no row covers
}

// OK is true when every legacy assignee is represented and none is unresolved.
func (r AssigneeGateReport) OK() bool {
	if len(r.Unresolved) > 0 {
		return false
	}
	for _, p := range r.Projects {
		if p.Copied != p.Legacy {
			return false
		}
	}
	return true
}

// AssigneeMigrationReport counts, per project, legacy assignees against the items
// that carry them in item_assignees, and lists values it could not resolve.
func (db *DB) AssigneeMigrationReport(ctx context.Context) (*AssigneeGateReport, error) {
	if has, err := hasColumn(db.conn, "items", "assignee"); err != nil {
		return nil, err
	} else if !has {
		return &AssigneeGateReport{}, nil
	}
	rows, err := db.conn.QueryContext(ctx,
		`SELECT p.key, COUNT(*),
		        SUM(CASE WHEN EXISTS (SELECT 1 FROM item_assignees a WHERE a.item_id = i.id) THEN 1 ELSE 0 END)
		 FROM items i JOIN projects p ON p.id = i.project_id
		 WHERE i.assignee IS NOT NULL AND TRIM(i.assignee) <> ''
		 GROUP BY p.key ORDER BY p.key`)
	if err != nil {
		return nil, err
	}
	rep := &AssigneeGateReport{}
	for rows.Next() {
		var p AssigneeGateProject
		if err := rows.Scan(&p.Project, &p.Legacy, &p.Copied); err != nil {
			rows.Close()
			return nil, err
		}
		rep.Projects = append(rep.Projects, p)
		rep.Legacy += p.Legacy
		rep.Copied += p.Copied
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	vals, err := db.conn.QueryContext(ctx,
		`SELECT DISTINCT assignee FROM items
		 WHERE assignee IS NOT NULL AND TRIM(assignee) <> ''
		   AND NOT EXISTS (SELECT 1 FROM item_assignees a WHERE a.item_id = items.id)`)
	if err != nil {
		return nil, err
	}
	defer vals.Close()
	for vals.Next() {
		var v string
		if err := vals.Scan(&v); err != nil {
			return nil, err
		}
		if _, ok := resolveLegacyAssignee(v); !ok {
			rep.Unresolved = append(rep.Unresolved, strings.TrimSpace(v))
		}
	}
	sort.Strings(rep.Unresolved)
	return rep, vals.Err()
}

// DropLegacyAssigneeColumn removes items.assignee, gated like the label column:
// it refuses unless AssigneeMigrationReport passes. Idempotent. Nothing may be
// reading the column when it goes, and a backup comes first.
func (db *DB) DropLegacyAssigneeColumn(ctx context.Context) (bool, error) {
	has, err := hasColumn(db.conn, "items", "assignee")
	if err != nil || !has {
		return false, err
	}
	rep, err := db.AssigneeMigrationReport(ctx)
	if err != nil {
		return false, err
	}
	if !rep.OK() {
		var bad []string
		for _, p := range rep.Projects {
			if p.Copied != p.Legacy {
				bad = append(bad, fmt.Sprintf("%s: %d legacy assignees, %d in item_assignees", p.Project, p.Legacy, p.Copied))
			}
		}
		for _, v := range rep.Unresolved {
			bad = append(bad, fmt.Sprintf("unresolved value %q", v))
		}
		return false, fmt.Errorf("refusing to drop items.assignee: the zero-loss check failed (%s)", strings.Join(bad, "; "))
	}
	if err := ensureColumnDropped(db.conn, "items", "assignee"); err != nil {
		return false, err
	}
	return true, nil
}
