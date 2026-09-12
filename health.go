package ledgercore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The ledger's own linter: what in its data is wrong, hidden, or stale.
//
// Every check here exists because the thing it looks for actually happened.
// Projects created by a mis-quoted argument, relations left pointing at
// deleted tickets, values outside the vocabulary from before it was enforced.
// Left alone, these are invisible: the lists don't show them, so nobody sees
// them until something reads oddly.
//
// Findings are reported, never repaired. Each one carries the exact command
// that fixes it, so the decision stays with a person who can tell a mistake
// from a deliberate state (docs/ledger.md U14).

// Finding is one thing worth a look.
type Finding struct {
	Check      string // stable identifier, e.g. "empty_project"
	EntityType string // project | item | item_relation | note | resource
	EntityID   string
	Title      string // what the thing is called, for a person reading the list
	Detail     string // what is wrong with it
	Fix        string // the command that resolves it, or "" when it needs a decision
}

// HealthFindings runs every check. Read-only: it writes nothing.
//
// Checks look at live rows. Deleted rows are history -- something already
// dealt with, kept as a record -- and a list that keeps reporting resolved
// things is a list people stop reading. Deletion is itself how several of
// these findings get fixed.
func (db *DB) HealthFindings(ctx context.Context) ([]Finding, error) {
	var all []Finding
	for _, check := range []func(context.Context) ([]Finding, error){
		db.checkEmptyProjects,
		db.checkItemsInDeletedProjects,
		db.checkDeletedParents,
		db.checkDanglingRelations,
		db.checkOffVocabulary,
		db.checkRunningTimers,
		db.checkMissingComponents,
	} {
		found, err := check(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, found...)
	}
	return all, nil
}

// A project with nothing in it is usually an accident -- a mis-typed argument
// that auto-created it (a project literally keyed "project=cortex-stack" once
// reached this database), not a project someone meant to start.
func (db *DB) checkEmptyProjects(ctx context.Context) ([]Finding, error) {
	return db.collect(ctx, `
		SELECT p.id, p.key, ''
		FROM projects p
		WHERE p.deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM items i WHERE i.project_id = p.id AND i.deleted_at IS NULL)
		ORDER BY p.key`,
		func(id, title, _ string) Finding {
			return Finding{
				Check: "empty_project", EntityType: "project", EntityID: id, Title: title,
				Detail: "No tickets. Often a project created by accident from a mis-typed argument.",
				Fix:    "ledger_delete entity_type=project id=" + id,
			}
		})
}

// Live tickets whose project was deleted: they belong to nothing, and no
// project listing will ever show them again.
func (db *DB) checkItemsInDeletedProjects(ctx context.Context) ([]Finding, error) {
	return db.collect(ctx, `
		SELECT i.id, i.title, p.key
		FROM items i JOIN projects p ON p.id = i.project_id
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NOT NULL
		ORDER BY p.key, i.created_at`,
		func(id, title, projectKey string) Finding {
			return Finding{
				Check: "item_in_deleted_project", EntityType: "item", EntityID: id, Title: title,
				Detail: fmt.Sprintf("Live ticket in deleted project %q, so no project listing shows it.", projectKey),
				Fix:    "ledger_restore entity_type=project id=<the project> — or delete the ticket",
			}
		})
}

// A live ticket under a deleted parent: it still appears in its project, but
// its place in the hierarchy points at something hidden.
func (db *DB) checkDeletedParents(ctx context.Context) ([]Finding, error) {
	return db.collect(ctx, `
		SELECT i.id, i.title, parent.title
		FROM items i JOIN items parent ON parent.id = i.parent_id
		WHERE i.deleted_at IS NULL AND parent.deleted_at IS NOT NULL
		ORDER BY i.created_at`,
		func(id, title, parentTitle string) Finding {
			return Finding{
				Check: "deleted_parent", EntityType: "item", EntityID: id, Title: title,
				Detail: fmt.Sprintf("Its parent %q is deleted.", parentTitle),
				Fix:    "ledger_restore entity_type=item id=<the parent> — or re-parent this ticket",
			}
		})
}

// Deleting a ticket doesn't touch its relations (no cascade, by design), so
// live relations can end up pointing at deleted tickets.
func (db *DB) checkDanglingRelations(ctx context.Context) ([]Finding, error) {
	return db.collect(ctx, `
		SELECT r.id, r.relation_type, COALESCE(f.title, '(missing)') || ' -> ' || COALESCE(t.title, '(missing)')
		FROM item_relations r
		LEFT JOIN items f ON f.id = r.from_item_id
		LEFT JOIN items t ON t.id = r.to_item_id
		WHERE r.deleted_at IS NULL
		  AND (f.id IS NULL OR t.id IS NULL OR f.deleted_at IS NOT NULL OR t.deleted_at IS NOT NULL)
		ORDER BY r.created_at`,
		func(id, relType, ends string) Finding {
			return Finding{
				Check: "dangling_relation", EntityType: "item_relation", EntityID: id, Title: relType,
				Detail: "Points at a deleted or missing ticket: " + ends,
				Fix:    "ledger_delete entity_type=item_relation id=" + id,
			}
		})
}

// Values outside the vocabulary. Impossible to write since enforcement landed
// (validation plus triggers), but rows from before it are still here, and a
// check that only passes because writing is blocked is worth keeping: it is
// what would catch a future path around this package.
func (db *DB) checkOffVocabulary(ctx context.Context) ([]Finding, error) {
	var out []Finding
	for _, c := range []struct{ table, column, entity string }{
		{"items", "status", "item"},
		{"items", "type", "item"},
		{"item_relations", "relation_type", "item_relation"},
		{"notes", "type", "note"},
	} {
		allowed := map[string][]string{
			"status": statuses, "type": itemTypes, "relation_type": relationTypes,
		}[c.column]
		if c.table == "notes" {
			allowed = noteTypes
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(allowed)), ",")
		args := make([]any, 0, len(allowed))
		for _, v := range allowed {
			args = append(args, v)
		}
		// Live rows only. A soft-deleted row with an off-vocabulary value is
		// history -- the four such relations in this database were resolved
		// deliberately and kept as a record. Reporting them for ever would
		// train the reader to ignore this list, which is the one thing it
		// cannot afford.
		//nolint:gosec // table/column are this package's own literals, never caller input
		q := fmt.Sprintf(`SELECT id, %s, ''
			FROM %s WHERE deleted_at IS NULL AND %s NOT IN (%s)`, c.column, c.table, c.column, placeholders)
		found, err := db.collectArgs(ctx, q, args, func(id, value, _ string) Finding {
			return Finding{
				Check: "off_vocabulary", EntityType: c.entity, EntityID: id, Title: value,
				Detail: fmt.Sprintf("%s.%s is %q, which is not in the vocabulary.", c.table, c.column, value),
				Fix:    "", // needs a decision: what did it mean?
			}
		})
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// A timer started and never stopped keeps accruing. Usually someone forgot.
func (db *DB) checkRunningTimers(ctx context.Context) ([]Finding, error) {
	return db.collect(ctx, `
		SELECT i.id, i.title, n.created_at
		FROM items i
		JOIN notes n ON n.item_id = i.id AND n.type = 'time-started' AND n.deleted_at IS NULL
		WHERE i.deleted_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM notes stop
			WHERE stop.item_id = i.id AND stop.type = 'time-ended'
			  AND stop.deleted_at IS NULL AND stop.created_at > n.created_at)
		ORDER BY n.created_at`,
		func(id, title, since string) Finding {
			return Finding{
				Check: "running_timer", EntityType: "item", EntityID: id, Title: title,
				Detail: "A timer has been running since " + since + ".",
				Fix:    "ledger_stop_timer item_id=" + id,
			}
		})
}

// Component is a copied string, not a reference, so the component it names
// can be renamed or deleted out from under it.
func (db *DB) checkMissingComponents(ctx context.Context) ([]Finding, error) {
	return db.collect(ctx, `
		SELECT i.id, i.title, i.component
		FROM items i
		WHERE i.deleted_at IS NULL AND i.component IS NOT NULL AND i.component != ''
		  AND NOT EXISTS (
			SELECT 1 FROM items c
			WHERE c.type = 'component' AND c.deleted_at IS NULL AND c.title = i.component)
		ORDER BY i.component`,
		func(id, title, component string) Finding {
			return Finding{
				Check: "missing_component", EntityType: "item", EntityID: id, Title: title,
				Detail: fmt.Sprintf("Names component %q, which no live component has as its title.", component),
				Fix:    "ledger_update_item id=" + id + " component=<an existing component, or empty to clear>",
			}
		})
}

func (db *DB) collect(ctx context.Context, query string, build func(a, b, c string) Finding) ([]Finding, error) {
	return db.collectArgs(ctx, query, nil, build)
}

func (db *DB) collectArgs(ctx context.Context, query string, args []any, build func(a, b, c string) Finding) ([]Finding, error) {
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("health check: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var a, b, c sql.NullString
		if err := rows.Scan(&a, &b, &c); err != nil {
			return nil, err
		}
		out = append(out, build(a.String, b.String, c.String))
	}
	return out, rows.Err()
}
