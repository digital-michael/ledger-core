package ledgercore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Copying projects from one ledger into another.
//
// The rules, decided with the operator on 2026-11-08:
//
//   - Ids are preserved. A link keeps its meaning on the target, and a second run is a
//     no-op: a row that is already there and identical is counted, not written.
//   - A project is copied whole (every ticket, note, resource, label, assignee, watcher,
//     member, relation and workflow in it, and the audit trail of all of those). A plan or a
//     workflow is copied as the whole projects it belongs to and points at.
//   - Links to projects that were not chosen are never silently dropped and never silently
//     followed. When any exist the copy stops and names them (CrossLinksError); the operator
//     then either includes named projects (WithLinked), or rejects every outside link
//     (DropCrossLinks, which is reported), or cancels by doing neither.
//   - Nothing on the target is overwritten: a row that is there and different is a conflict,
//     and a conflict stops the whole copy. The copy is one target transaction, so it is all
//     or nothing; a dry run is the same transaction rolled back, which also means it meets
//     every constraint and vocabulary rule the real run would.
//   - Every copied row is read back from the target and compared with what was meant to be
//     written before the transaction is allowed to commit.

// CopyScope says what to copy.
type CopyScope struct {
	All            bool     // every live project
	Projects       []string // project keys, old keys or ids
	Plans          []string // ids of plan tickets: their project and the projects of their members
	Workflows      []string // workflow ids: their project and the projects of what they associate with or link to
	IncludeDeleted bool     // also soft-deleted projects (a deleted ticket in a live project is always copied, as deleted)
}

// CopyOptions configures CopyLedger.
type CopyOptions struct {
	Scope CopyScope

	// WithLinked adds these projects to the selection (the operator's answer to a
	// CrossLinksError). Links that still point outside the final selection are left out.
	WithLinked []string
	// DropCrossLinks leaves out every link to a project that is not selected, and reports them.
	DropCrossLinks bool

	// MapIdentity rewrites identities on the way: exact values, "person:alice" to
	// "person:alice-remote". Applied to every column that holds an identity.
	MapIdentity map[string]string

	// SkipInvalid leaves out rows whose value is outside the vocabulary (written before the rules
	// existed), when they are leaf rows: a relation, note, workflow link or project member. They are
	// named in the report. A ticket or workflow outside the vocabulary is never skipped.
	SkipInvalid bool

	// Apply commits. Without it the copy runs fully and is rolled back.
	Apply bool
}

// CopyTable is one table's line in the report.
type CopyTable struct {
	Table        string
	Selected     int // rows chosen for the copy
	Inserted     int // rows written (or, in a dry run, that would be)
	Updated      int // rows whose only difference was a parent link left out by an earlier copy, now filled in
	AlreadyThere int // rows already on the target, identical
}

// CrossLink counts links between the selection and one project outside it.
type CrossLink struct {
	Project   string // that project's key
	Kind      string // "relation (blocked_by)", "parent", "workflow link", "workflow association"
	Direction string // "out": a selected row points at the other project; "in": the other project points here
	Count     int
}

// CopyReport says what a copy did, or in a dry run would do.
type CopyReport struct {
	Projects         []string // keys of the projects copied
	Tables           []CopyTable
	Dropped          []CrossLink // links left out because their other end was not selected
	IdentitiesMapped int
	Skipped          []string // rows left out under SkipInvalid, in words
	Applied          bool
}

// VocabularyError is returned when rows hold a value the ledger's rules no longer allow, which
// the target would refuse. Every such row is listed. Nothing was written.
type VocabularyError struct {
	Rows      []string // "table pk: column = value"
	Skippable bool     // true when every one is a leaf row that -skip-invalid may leave out
}

func (e *VocabularyError) Error() string {
	shown, more := e.Rows, ""
	if len(shown) > 20 {
		more = fmt.Sprintf(" (and %d more)", len(shown)-20)
		shown = shown[:20]
	}
	msg := fmt.Sprintf("%d row(s) hold a value outside the ledger vocabulary, which the target refuses: %s%s. Nothing was copied.",
		len(e.Rows), strings.Join(shown, "; "), more)
	if e.Skippable {
		return msg + " They are leaf rows: fix them in the source, or leave them out with -skip-invalid (they are listed in the report)."
	}
	return msg + " Some are tickets or workflows, which cannot be left out: fix them in the source."
}

// leafTables hold rows nothing else depends on, so a row may be left out without orphaning anything.
var leafTables = map[string]bool{"item_relations": true, "notes": true, "workflow_links": true, "project_members": true}

// checkVocabulary refuses (or, with SkipInvalid and only leaf rows, removes) rows the target's rules
// would reject, together with the audit rows of the ones removed.
func (c *copier) checkVocabulary(d *copyData) error {
	var bad []string
	skippable := true
	drop := map[string]map[string]bool{}
	for _, rule := range vocabRules() {
		ok := map[string]bool{}
		for _, v := range rule.values {
			ok[v] = true
		}
		for _, r := range d.rows[rule.table] {
			v, isStr := r.vals[rule.column].(string)
			if r.vals[rule.column] == nil || (isStr && ok[v]) {
				continue
			}
			bad = append(bad, fmt.Sprintf("%s %s: %s = %v", rule.table, r.pk, rule.column, r.vals[rule.column]))
			if !leafTables[rule.table] {
				skippable = false
			}
			if drop[rule.table] == nil {
				drop[rule.table] = map[string]bool{}
			}
			drop[rule.table][r.pk] = true
		}
	}
	if len(bad) == 0 {
		return nil
	}
	if !c.o.SkipInvalid || !skippable {
		return &VocabularyError{Rows: bad, Skippable: skippable}
	}
	gone := map[string]bool{} // audit entity_type + id
	for table, pks := range drop {
		var kept []copyRow
		for _, r := range d.rows[table] {
			if pks[r.pk] {
				gone[auditType[table]+"\x00"+r.pk] = true
			} else {
				kept = append(kept, r)
			}
		}
		d.rows[table] = kept
	}
	var audit []copyRow
	for _, r := range d.rows["audit_log"] {
		et, _ := r.vals["entity_type"].(string)
		id, _ := r.vals["entity_id"].(string)
		if !gone[et+"\x00"+id] {
			audit = append(audit, r)
		}
	}
	d.rows["audit_log"] = audit
	c.skipped = bad
	return nil
}

// CrossLinksError is returned when the selection is linked to projects outside it and the
// operator has not said what to do. Nothing was written.
type CrossLinksError struct{ Links []CrossLink }

func (e *CrossLinksError) Error() string {
	byProject := map[string][]string{}
	var order []string
	for _, l := range e.Links {
		if _, ok := byProject[l.Project]; !ok {
			order = append(order, l.Project)
		}
		byProject[l.Project] = append(byProject[l.Project], fmt.Sprintf("%s %s %d", l.Kind, l.Direction, l.Count))
	}
	var parts []string
	for _, p := range order {
		parts = append(parts, fmt.Sprintf("%s (%s)", p, strings.Join(byProject[p], "; ")))
	}
	return "the selection is linked to projects that were not chosen: " + strings.Join(parts, ", ") +
		". Nothing was copied. Include some of them with -with-linked name[,name], or leave every outside link out with -drop-cross-links; changing neither cancels."
}

// CopyConflictError is returned when rows on the target differ from the source. Nothing was written.
type CopyConflictError struct{ Conflicts []string }

func (e *CopyConflictError) Error() string {
	shown := e.Conflicts
	more := ""
	if len(shown) > 10 {
		more = fmt.Sprintf(" (and %d more)", len(shown)-10)
		shown = shown[:10]
	}
	return fmt.Sprintf("%d row(s) on the target differ from the source, so nothing was copied (a copy never overwrites): %s%s",
		len(e.Conflicts), strings.Join(shown, "; "), more)
}

// copyOrder is the order tables are written in: what is referenced before what references it.
var copyOrder = []string{
	"projects", "project_aliases", "project_members", "project_watchers",
	"items", "item_labels", "item_assignees", "item_watchers",
	"notes", "resources", "item_relations",
	"workflows", "workflow_versions", "workflow_associations", "workflow_links",
	"audit_log",
}

// identityColumns are the columns that hold a person, agent or program.
var identityColumns = map[string][]string{
	"projects":          {"created_by"},
	"items":             {"created_by", "owner"},
	"notes":             {"author"},
	"item_labels":       {"created_by"},
	"item_assignees":    {"member", "created_by"},
	"item_watchers":     {"member", "created_by"},
	"project_watchers":  {"member", "created_by"},
	"project_members":   {"member", "created_by"},
	"workflows":         {"created_by"},
	"workflow_versions": {"created_by"},
	"audit_log":         {"actor"},
}

// auditType is the audit_log entity_type of the rows of a table, where it has one.
var auditType = map[string]string{
	"projects": "project", "items": "item", "notes": "note", "resources": "resource",
	"item_relations": "item_relation", "item_labels": "item_label", "item_assignees": "item_assignee",
	"item_watchers": "item_watcher", "project_watchers": "project_watcher", "project_members": "project_member",
	"workflows": "workflow", "workflow_associations": "workflow_association", "workflow_links": "workflow_link",
}

var identName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

type copyRow struct {
	pk   string
	vals map[string]any
}

// CopyLedger copies the selected projects from src into dst. See the rules above.
func CopyLedger(ctx context.Context, srcStore, dstStore Store, o CopyOptions) (*CopyReport, error) {
	c, err := newCopier(ctx, srcStore, dstStore, o)
	if err != nil {
		return nil, err
	}
	return c.run()
}

// newCopier checks the request and returns the copier for it. A copy and a verification
// make exactly the same checks, so the same selection means the same thing to both.
func newCopier(ctx context.Context, srcStore, dstStore Store, o CopyOptions) (*copier, error) {
	src, ok1 := srcStore.(*DB)
	dst, ok2 := dstStore.(*DB)
	if !ok1 || !ok2 {
		return nil, errors.New("copy needs two ledgers opened by this package")
	}
	if src == dst || src.conn.DB == dst.conn.DB {
		return nil, errors.New("the source and the target are the same ledger")
	}
	if len(o.WithLinked) > 0 && o.DropCrossLinks {
		return nil, errors.New("choose one: include named linked projects (-with-linked) or leave every outside link out (-drop-cross-links)")
	}
	s := o.Scope
	if !s.All && len(s.Projects) == 0 && len(s.Plans) == 0 && len(s.Workflows) == 0 {
		return nil, errors.New("nothing is selected: name projects, plans or workflows, or ask for everything")
	}
	for from, to := range o.MapIdentity {
		for _, id := range []string{from, to} {
			if _, err := ParseIdentity(FieldCreatedBy, id); err != nil {
				return nil, fmt.Errorf("identity mapping %q -> %q: %w", from, to, err)
			}
		}
	}

	return &copier{ctx: ctx, src: src, dst: dst, o: o}, nil
}

type copier struct {
	ctx      context.Context
	src, dst *DB
	o        CopyOptions

	keys      map[string]string // project id -> key, every project in the source
	deleted   map[string]bool   // project id -> soft-deleted
	selected  map[string]bool   // project ids to copy
	dropLinks bool
	skipped   []string
}

// prepare resolves the selection, enforces the decision about outside links, and reads
// everything that is to be copied. It writes nothing anywhere.
func (c *copier) prepare() (*CopyReport, *copyData, error) {
	if err := c.loadProjectNames(); err != nil {
		return nil, nil, err
	}
	p0, err := c.resolveScope()
	if err != nil {
		return nil, nil, err
	}

	// 1. Links that leave the selection need a decision before anything else happens.
	if links, err := c.outsideLinks(p0, true); err != nil {
		return nil, nil, err
	} else if len(links) > 0 && len(c.o.WithLinked) == 0 && !c.o.DropCrossLinks {
		return nil, nil, &CrossLinksError{Links: links}
	}
	c.selected = p0
	for _, ref := range c.o.WithLinked {
		id, err := c.resolveProject(ref)
		if err != nil {
			return nil, nil, fmt.Errorf("-with-linked %q: %w", ref, err)
		}
		c.selected[id] = true
	}
	dropped, err := c.outsideLinks(c.selected, false)
	if err != nil {
		return nil, nil, err
	}

	rep := &CopyReport{Dropped: dropped}
	for id := range c.selected {
		rep.Projects = append(rep.Projects, c.keys[id])
	}
	sort.Strings(rep.Projects)

	// 2. Read everything that is to be copied.
	data, err := c.load()
	if err != nil {
		return nil, nil, err
	}
	rep.IdentitiesMapped = c.mapIdentities(data)
	rep.Skipped = c.skipped
	return rep, data, nil
}

func (c *copier) run() (*CopyReport, error) {
	rep, data, err := c.prepare()
	if err != nil {
		return nil, err
	}

	// 3. Write it in one target transaction, verify, and commit only if asked.
	tx, err := c.dst.conn.BeginTx(c.ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var conflicts []string
	for _, table := range copyOrder {
		rows := data.rows[table]
		line := CopyTable{Table: table, Selected: len(rows)}
		cols := data.cols[table]
		for _, r := range rows {
			existing, err := c.dstRow(tx, table, cols, r.pk)
			if err != nil {
				return nil, err
			}
			if existing != nil {
				switch diff := differs(cols, r.vals, existing); {
				case diff == "":
					line.AlreadyThere++
				case isParentFillIn(table, cols, r.vals, existing):
					// An earlier copy left this ticket's parent out because the parent's project was
					// not selected; the parent is selected now, so the link is completed. This is the
					// only kind of change a copy makes to a row that is already there.
					if err := c.fillParent(tx, r); err != nil {
						return nil, fmt.Errorf("completing the parent of %s: %w", r.pk, err)
					}
					line.Updated++
				default:
					conflicts = append(conflicts, fmt.Sprintf("%s %s: %s", table, r.pk, diff))
				}
				continue
			}
			if err := c.insert(tx, table, cols, r); err != nil {
				return nil, fmt.Errorf("copying %s %s: %w", table, r.pk, err)
			}
			line.Inserted++
		}
		rep.Tables = append(rep.Tables, line)
	}
	if len(conflicts) > 0 {
		return nil, &CopyConflictError{Conflicts: conflicts}
	}
	if err := c.verify(tx, data); err != nil {
		return nil, err
	}
	if !c.o.Apply {
		return rep, nil // the deferred Rollback leaves the target as it was
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	rep.Applied = true
	return rep, nil
}

// ---- choosing projects ----

func (c *copier) loadProjectNames() error {
	c.keys, c.deleted = map[string]string{}, map[string]bool{}
	rows, err := c.src.conn.QueryContext(c.ctx, `SELECT id, key, deleted_at IS NOT NULL FROM projects`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key string
		var del bool
		if err := rows.Scan(&id, &key, &del); err != nil {
			return err
		}
		c.keys[id], c.deleted[id] = key, del
	}
	return rows.Err()
}

// resolveProject finds a project by key, old key, id or unique name, and refuses a
// soft-deleted one unless deleted projects were asked for.
func (c *copier) resolveProject(ref string) (string, error) {
	p, deletedAt, err := c.src.resolveProject(c.ctx, ref)
	if err != nil {
		return "", err
	}
	if deletedAt && !c.o.Scope.IncludeDeleted {
		return "", fmt.Errorf("project %q is deleted (ask for deleted projects to copy it)", p.Key)
	}
	return p.ID, nil
}

func (c *copier) resolveScope() (map[string]bool, error) {
	sel := map[string]bool{}
	s := c.o.Scope
	if s.All {
		for id := range c.keys {
			if !c.deleted[id] || s.IncludeDeleted {
				sel[id] = true
			}
		}
	}
	for _, ref := range s.Projects {
		id, err := c.resolveProject(ref)
		if err != nil {
			return nil, err
		}
		sel[id] = true
	}
	for _, ref := range s.Plans {
		ids, err := c.planProjects(ref)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			sel[id] = true
		}
	}
	for _, ref := range s.Workflows {
		ids, err := c.workflowProjects(ref)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			sel[id] = true
		}
	}
	if len(sel) == 0 {
		return nil, errors.New("the selection contains no projects")
	}
	return sel, nil
}

// planProjects: the plan's own project, and the project of every ticket that is part of it.
func (c *copier) planProjects(ref string) ([]string, error) {
	id, err := c.src.resolveItemID(c.ctx, ref)
	if err != nil {
		return nil, err
	}
	var typ, project string
	if err := c.src.conn.QueryRowContext(c.ctx, `SELECT type, project_id FROM items WHERE id = ?`, id).Scan(&typ, &project); err != nil {
		return nil, err
	}
	if typ != "plan" {
		return nil, fmt.Errorf("ticket %s is a %s, not a plan", id, typ)
	}
	out := []string{project}
	rows, err := c.src.conn.QueryContext(c.ctx, `SELECT DISTINCT i.project_id FROM item_relations r JOIN items i ON i.id = r.from_item_id
		WHERE r.to_item_id = ? AND r.relation_type = 'part_of' AND r.deleted_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return c.liveOnly(out), rows.Err()
}

// workflowProjects: the workflow's project, and the project of every ticket it is associated with or linked to.
func (c *copier) workflowProjects(ref string) ([]string, error) {
	var id, project string
	err := c.src.conn.QueryRowContext(c.ctx, `SELECT id, project_id FROM workflows WHERE id = ?`, ref).Scan(&id, &project)
	if err == sql.ErrNoRows {
		return nil, &lookupError{msg: fmt.Sprintf("workflow %q not found", ref), kind: ErrNotFound}
	}
	if err != nil {
		return nil, err
	}
	out := []string{project}
	for _, q := range []string{
		`SELECT DISTINCT i.project_id FROM workflow_associations a JOIN items i ON i.id = a.item_id WHERE a.workflow_id = ? AND a.deleted_at IS NULL`,
		`SELECT DISTINCT i.project_id FROM workflow_links a JOIN items i ON i.id = a.item_id WHERE a.workflow_id = ? AND a.deleted_at IS NULL`,
	} {
		rows, err := c.src.conn.QueryContext(c.ctx, q, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, p)
		}
		rows.Close()
	}
	return c.liveOnly(out), nil
}

// liveOnly drops soft-deleted projects unless they were asked for.
func (c *copier) liveOnly(ids []string) []string {
	if c.o.Scope.IncludeDeleted {
		return ids
	}
	var out []string
	for _, id := range ids {
		if !c.deleted[id] {
			out = append(out, id)
		}
	}
	return out
}

// ---- links between the selection and everything else ----

// outsideLinks counts every link that has exactly one end in the selection: a relation, a
// parent, a workflow association or a workflow link. liveOnly counts the links a person can
// see (for the notification); otherwise it counts every row that cannot be copied.
func (c *copier) outsideLinks(sel map[string]bool, liveOnly bool) ([]CrossLink, error) {
	type k struct{ project, kind, dir string }
	counts := map[k]int{}
	add := func(inA, inB bool, projA, projB, kind string) {
		if inA == inB {
			return
		}
		if inA {
			counts[k{c.keys[projB], kind, "out"}]++
		} else {
			counts[k{c.keys[projA], kind, "in"}]++
		}
	}
	live := func(col string) string {
		if liveOnly {
			return " AND " + col + " IS NULL"
		}
		return ""
	}
	queries := []struct {
		sql  string
		kind string
	}{
		{`SELECT fa.project_id, tb.project_id, r.relation_type FROM item_relations r
		  JOIN items fa ON fa.id = r.from_item_id JOIN items tb ON tb.id = r.to_item_id
		  WHERE fa.project_id <> tb.project_id` + live("r.deleted_at"), "relation"},
		{`SELECT c.project_id, p.project_id, '' FROM items c JOIN items p ON p.id = c.parent_id
		  WHERE c.project_id <> p.project_id` + live("c.deleted_at"), "parent"},
		{`SELECT w.project_id, i.project_id, '' FROM workflow_links l JOIN workflows w ON w.id = l.workflow_id JOIN items i ON i.id = l.item_id
		  WHERE w.project_id <> i.project_id` + live("l.deleted_at"), "workflow link"},
		{`SELECT w.project_id, i.project_id, '' FROM workflow_associations l JOIN workflows w ON w.id = l.workflow_id JOIN items i ON i.id = l.item_id
		  WHERE w.project_id <> i.project_id` + live("l.deleted_at"), "workflow association"},
	}
	for _, qq := range queries {
		rows, err := c.src.conn.QueryContext(c.ctx, qq.sql)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a, b, sub string
			if err := rows.Scan(&a, &b, &sub); err != nil {
				rows.Close()
				return nil, err
			}
			kind := qq.kind
			if sub != "" {
				kind += " (" + sub + ")"
			}
			add(sel[a], sel[b], a, b, kind)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	var out []CrossLink
	for key, n := range counts {
		out = append(out, CrossLink{Project: key.project, Kind: key.kind, Direction: key.dir, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Direction < b.Direction
	})
	return out, nil
}

// ---- reading the source ----

type copyData struct {
	rows map[string][]copyRow
	cols map[string][]string // the columns copied, per table: those both ledgers have
}

func (c *copier) tableColumns(db *DB, table string) ([]columnInfo, error) {
	return db.conn.d.columns(c.ctx, db.conn.DB, table)
}

// load reads every row to be copied.
func (c *copier) load() (*copyData, error) {
	d := &copyData{rows: map[string][]copyRow{}, cols: map[string][]string{}}
	pks := map[string]string{}
	srcCols := map[string][]string{}
	for _, table := range copyOrder {
		sc, err := c.tableColumns(c.src, table)
		if err != nil {
			return nil, err
		}
		dc, err := c.tableColumns(c.dst, table)
		if err != nil {
			return nil, err
		}
		pk, err := primaryKey(dc, table)
		if err != nil {
			return nil, err
		}
		pks[table] = pk
		inSrc := map[string]bool{}
		for _, col := range sc {
			inSrc[col.Name] = true
			srcCols[table] = append(srcCols[table], col.Name)
		}
		for _, col := range dc {
			if inSrc[col.Name] {
				d.cols[table] = append(d.cols[table], col.Name)
			}
		}
	}

	ids := map[string]map[string]bool{} // table -> selected primary keys
	set := func(table string, rows []copyRow) {
		d.rows[table] = rows
		ids[table] = map[string]bool{}
		for _, r := range rows {
			ids[table][r.pk] = true
		}
	}
	keyList := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	pids := keyList(c.selected)

	// Projects and the small tables that hang off a project.
	for _, table := range []string{"projects"} {
		rows, err := c.selectRows(table, srcCols[table], pks[table], "id", pids)
		if err != nil {
			return nil, err
		}
		set(table, rows)
	}
	for _, table := range []string{"project_aliases", "project_members", "project_watchers"} {
		rows, err := c.selectRows(table, srcCols[table], pks[table], "project_id", pids)
		if err != nil {
			return nil, err
		}
		set(table, rows)
	}
	// Items, parents first; a parent outside the selection is cut (and was reported).
	itemRows, err := c.selectRows("items", srcCols["items"], pks["items"], "project_id", pids)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range itemRows {
		have[r.pk] = true
	}
	for _, r := range itemRows {
		if p, _ := r.vals["parent_id"].(string); p != "" && !have[p] {
			r.vals["parent_id"] = nil
		}
	}
	set("items", orderParentsFirst(itemRows))
	iids := keyList(ids["items"])

	for _, table := range []string{"item_labels", "item_assignees", "item_watchers"} {
		rows, err := c.selectRows(table, srcCols[table], pks[table], "item_id", iids)
		if err != nil {
			return nil, err
		}
		set(table, rows)
	}
	for _, table := range []string{"notes", "resources"} {
		byItem, err := c.selectRows(table, srcCols[table], pks[table], "item_id", iids)
		if err != nil {
			return nil, err
		}
		byProject, err := c.selectRowsWhere(table, srcCols[table], pks[table], "item_id IS NULL AND project_id", pids)
		if err != nil {
			return nil, err
		}
		set(table, append(byItem, byProject...))
	}
	rels, err := c.selectRows("item_relations", srcCols["item_relations"], pks["item_relations"], "from_item_id", iids)
	if err != nil {
		return nil, err
	}
	var keep []copyRow
	for _, r := range rels {
		if to, _ := r.vals["to_item_id"].(string); ids["items"][to] {
			keep = append(keep, r)
		}
	}
	set("item_relations", keep)

	wfs, err := c.selectRows("workflows", srcCols["workflows"], pks["workflows"], "project_id", pids)
	if err != nil {
		return nil, err
	}
	set("workflows", wfs)
	wids := keyList(ids["workflows"])
	versions, err := c.selectRows("workflow_versions", srcCols["workflow_versions"], pks["workflow_versions"], "workflow_id", wids)
	if err != nil {
		return nil, err
	}
	set("workflow_versions", versions)
	for _, table := range []string{"workflow_associations", "workflow_links"} {
		rows, err := c.selectRows(table, srcCols[table], pks[table], "workflow_id", wids)
		if err != nil {
			return nil, err
		}
		var kept []copyRow
		for _, r := range rows {
			if it, _ := r.vals["item_id"].(string); ids["items"][it] {
				kept = append(kept, r)
			}
		}
		set(table, kept)
	}

	// The audit trail of everything that was copied.
	var audit []copyRow
	for _, table := range copyOrder {
		et, ok := auditType[table]
		if !ok || len(ids[table]) == 0 {
			continue
		}
		rows, err := c.selectRowsWhere("audit_log", srcCols["audit_log"], pks["audit_log"], "entity_type = '"+et+"' AND entity_id", keyList(ids[table]))
		if err != nil {
			return nil, err
		}
		audit = append(audit, rows...)
	}
	sort.SliceStable(audit, func(i, j int) bool {
		a, b := audit[i].vals["created_at"].(string), audit[j].vals["created_at"].(string)
		if a != b {
			return a < b
		}
		return audit[i].pk < audit[j].pk
	})
	set("audit_log", audit)

	if err := c.checkVocabulary(d); err != nil {
		return nil, err
	}

	// Data in a column the target does not have would be lost: refuse instead.
	for _, table := range copyOrder {
		onTarget := map[string]bool{}
		for _, col := range d.cols[table] {
			onTarget[col] = true
		}
		for _, col := range srcCols[table] {
			if onTarget[col] {
				continue
			}
			for _, r := range d.rows[table] {
				if r.vals[col] != nil {
					return nil, fmt.Errorf("column %s.%s exists only in the source and holds data (row %s); copying would lose it", table, col, r.pk)
				}
			}
		}
	}
	return d, nil
}

const inChunk = 400

func (c *copier) selectRows(table string, cols []string, pk, by string, values []string) ([]copyRow, error) {
	return c.selectRowsWhere(table, cols, pk, by, values)
}

// selectRowsWhere reads rows where <by> IN (values), in chunks.
func (c *copier) selectRowsWhere(table string, cols []string, pk, by string, values []string) ([]copyRow, error) {
	for _, n := range append([]string{table}, cols...) {
		if !identName.MatchString(n) {
			return nil, fmt.Errorf("unexpected identifier %q", n)
		}
	}
	var out []copyRow
	for i := 0; i < len(values); i += inChunk {
		end := i + inChunk
		if end > len(values) {
			end = len(values)
		}
		chunk := values[i:end]
		args := make([]any, len(chunk))
		for j, v := range chunk {
			args[j] = v
		}
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s IN (%s)`, strings.Join(cols, ", "), table, by, placeholders(len(chunk)))
		rows, err := c.src.conn.QueryContext(c.ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", table, err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for j := range vals {
				ptrs[j] = &vals[j]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			r := copyRow{vals: map[string]any{}}
			for j, col := range cols {
				r.vals[col] = norm(vals[j])
			}
			r.pk = fmt.Sprint(r.vals[pk])
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// orderParentsFirst orders items so a parent is always written before its children.
func orderParentsFirst(rows []copyRow) []copyRow {
	byID := map[string]copyRow{}
	for _, r := range rows {
		byID[r.pk] = r
	}
	depth := map[string]int{}
	var depthOf func(id string, seen map[string]bool) int
	depthOf = func(id string, seen map[string]bool) int {
		if d, ok := depth[id]; ok {
			return d
		}
		p, _ := byID[id].vals["parent_id"].(string)
		d := 0
		if p != "" && byID[p].pk != "" && !seen[p] {
			seen[id] = true
			d = depthOf(p, seen) + 1
		}
		depth[id] = d
		return d
	}
	out := append([]copyRow(nil), rows...)
	for _, r := range out {
		depthOf(r.pk, map[string]bool{})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if depth[out[i].pk] != depth[out[j].pk] {
			return depth[out[i].pk] < depth[out[j].pk]
		}
		return out[i].vals["created_at"].(string) < out[j].vals["created_at"].(string)
	})
	return out
}

// norm gives a value one shape whichever database it came from.
func norm(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case int16:
		return int64(x)
	case bool:
		if x {
			return int64(1)
		}
		return int64(0)
	}
	return v
}

// mapIdentities rewrites identity columns by the operator's mapping and counts the values changed.
func (c *copier) mapIdentities(d *copyData) int {
	if len(c.o.MapIdentity) == 0 {
		return 0
	}
	n := 0
	for table, cols := range identityColumns {
		for _, r := range d.rows[table] {
			for _, col := range cols {
				if s, ok := r.vals[col].(string); ok {
					if to, ok := c.o.MapIdentity[s]; ok && to != s {
						r.vals[col] = to
						n++
					}
				}
			}
		}
	}
	return n
}

// ---- writing and checking the target ----

// dstRow reads one target row by primary key; nil if there is none.
func (c *copier) dstRow(tx *sql.Tx, table string, cols []string, pkValue string) (map[string]any, error) {
	pk, err := c.pkOf(table)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(c.ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, strings.Join(cols, ", "), table, pk), pkValue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for i, col := range cols {
		out[col] = norm(vals[i])
	}
	return out, nil
}

func (c *copier) pkOf(table string) (string, error) {
	cols, err := c.tableColumns(c.dst, table)
	if err != nil {
		return "", err
	}
	return primaryKey(cols, table)
}

func (c *copier) insert(tx *sql.Tx, table string, cols []string, r copyRow) error {
	args := make([]any, len(cols))
	for i, col := range cols {
		args[i] = r.vals[col]
	}
	_, err := tx.ExecContext(c.ctx, fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, strings.Join(cols, ", "), placeholders(len(cols))), args...)
	return err
}

// isParentFillIn reports whether the only difference between the source row and the
// target row is a parent link the target lacks.
func isParentFillIn(table string, cols []string, want, got map[string]any) bool {
	if table != "items" {
		return false
	}
	diffs := 0
	for _, col := range cols {
		if !reflect.DeepEqual(want[col], got[col]) {
			if col != "parent_id" || got[col] != nil || want[col] == nil {
				return false
			}
			diffs++
		}
	}
	return diffs == 1
}

func (c *copier) fillParent(tx *sql.Tx, r copyRow) error {
	_, err := tx.ExecContext(c.ctx, `UPDATE items SET parent_id = ? WHERE id = ? AND parent_id IS NULL`, r.vals["parent_id"], r.pk)
	return err
}

// differs names the first column where two rows disagree, or returns "".
func differs(cols []string, want, got map[string]any) string {
	for _, col := range cols {
		if !reflect.DeepEqual(want[col], got[col]) {
			return fmt.Sprintf("%s is %v on the target, %v in the source", col, show(got[col]), show(want[col]))
		}
	}
	return ""
}

func show(v any) string {
	if v == nil {
		return "NULL"
	}
	s := fmt.Sprint(v)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return fmt.Sprintf("%q", s)
}

// verify reads every copied row back from the target and compares it with what was meant to be there.
func (c *copier) verify(tx *sql.Tx, d *copyData) error {
	for _, table := range copyOrder {
		cols := d.cols[table]
		for _, r := range d.rows[table] {
			got, err := c.dstRow(tx, table, cols, r.pk)
			if err != nil {
				return err
			}
			if got == nil {
				return fmt.Errorf("verification failed: %s %s is missing from the target; nothing was committed", table, r.pk)
			}
			if diff := differs(cols, r.vals, got); diff != "" {
				return fmt.Errorf("verification failed: %s %s: %s; nothing was committed", table, r.pk, diff)
			}
		}
	}
	return nil
}

// ---- verifying a copy afterwards ----

// VerifyTable is one table's line in a verification.
type VerifyTable struct {
	Table        string
	Selected     int      // rows the source says belong to the selection
	Matching     int      // of those, present on the target and identical
	Missing      int      // not on the target
	Different    int      // on the target, but not the same
	OnlyOnTarget int      // rows on the target inside the same projects that the source does not have
	Examples     []string // the first few problems, in words
}

// VerifyReport says whether the target holds the selection exactly as the source does.
type VerifyReport struct {
	Projects []string
	Tables   []VerifyTable
	Dropped  []CrossLink // links the selection leaves out, as in a copy
	Skipped  []string    // rows left out under SkipInvalid, as in a copy
	// OK is true when every selected row is on the target and identical. Rows that exist
	// only on the target are reported but are not a failure: the target may have moved on.
	OK bool
}

// VerifyCopy compares the target with the source for the same selection a copy would make,
// and writes nothing to either. It makes the same decisions a copy does (the same outside-link
// choice, the same identity mapping), so run it with the options the copy was run with. It reads
// both ledgers afresh, which is the point: it confirms what is really there now.
func VerifyCopy(ctx context.Context, srcStore, dstStore Store, o CopyOptions) (*VerifyReport, error) {
	c, err := newCopier(ctx, srcStore, dstStore, o)
	if err != nil {
		return nil, err
	}
	rep, want, err := c.prepare()
	if err != nil {
		return nil, err
	}
	// What the target holds inside the same projects, read the same way. Ids are preserved, so
	// the selection is the same set of project ids.
	t := &copier{ctx: ctx, src: c.dst, dst: c.src, o: c.o, keys: c.keys, deleted: c.deleted, selected: c.selected}
	have, err := t.load()
	if err != nil {
		return nil, fmt.Errorf("reading the target: %w", err)
	}

	out := &VerifyReport{Projects: rep.Projects, Dropped: rep.Dropped, Skipped: rep.Skipped, OK: true}
	for _, table := range copyOrder {
		cols := want.cols[table]
		onTarget := map[string]copyRow{}
		for _, r := range have.rows[table] {
			onTarget[r.pk] = r
		}
		line := VerifyTable{Table: table, Selected: len(want.rows[table])}
		note := func(msg string) {
			if len(line.Examples) < 5 {
				line.Examples = append(line.Examples, msg)
			}
		}
		seen := map[string]bool{}
		for _, r := range want.rows[table] {
			seen[r.pk] = true
			got, ok := onTarget[r.pk]
			switch {
			case !ok:
				line.Missing++
				note(fmt.Sprintf("%s %s is not on the target", table, r.pk))
			case differs(cols, r.vals, got.vals) != "":
				line.Different++
				note(fmt.Sprintf("%s %s: %s", table, r.pk, differs(cols, r.vals, got.vals)))
			default:
				line.Matching++
			}
		}
		for pk := range onTarget {
			if !seen[pk] {
				line.OnlyOnTarget++
			}
		}
		if line.Missing > 0 || line.Different > 0 {
			out.OK = false
		}
		out.Tables = append(out.Tables, line)
	}
	return out, nil
}
