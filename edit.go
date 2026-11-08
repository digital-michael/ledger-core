package ledgercore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// One place an item is edited.
//
// Item edits used to live in four methods, each opening its own transaction:
// fields, status, priority, assignee. A person editing a ticket changes
// several of those at once, and none of the four could tell that something
// else had changed the ticket first. Both of those are the same problem --
// an edit is not one field -- so there is now one method, and the four are
// thin wrappers over it.
//
// Audit keeps the vocabulary it already had: a status change is still
// status_changed, a priority change still priority_changed. One save that
// touches three kinds of field writes three audit rows sharing a batch id,
// so history stays as fine-grained as it was while still reading as one act.

// ItemFields is what an edit may change. A nil pointer leaves the field
// alone; a pointer to "" clears it, for the fields that can be empty.
//
// Priority has no clear: it could not be cleared before this method existed
// either, and inventing the ability here would be a silent change in
// behaviour. Parent is absent on purpose -- re-parenting belongs with the
// tree view, and is classified privileged (see policy.go).
type ItemFields struct {
	Title       *string
	Description *string
	Label       *string   // replaces the whole label set with this one value ("" clears)
	Labels      *[]string // replaces the whole label set with exactly these (empty clears); not with Label
	Component   *string
	Status      *string
	Assignee    *string
	Priority    *int64
	Type        *string // privileged: changes what a ticket is, after the fact
}

// ItemUpdate is an edit, optionally conditional.
type ItemUpdate struct {
	Fields ItemFields

	// ExpectedUpdatedAt makes the write conditional: if the item's
	// updated_at is not this, the edit is refused with a *ConflictError and
	// the current item is returned alongside it, so the caller can show what
	// happened without reading again. Empty means "write regardless", which
	// is what the MCP tools do.
	ExpectedUpdatedAt string
}

func (f ItemFields) empty() bool {
	return f.Title == nil && f.Description == nil && f.Label == nil && f.Labels == nil && f.Component == nil &&
		f.Status == nil && f.Assignee == nil && f.Priority == nil && f.Type == nil
}

// UpdateItemFields applies an edit in one transaction. It writes only fields
// that were given and actually differ, so saving a form does not record
// changes nobody made.
func (db *DB) UpdateItemFields(ctx context.Context, id string, u ItemUpdate) (*Item, error) {
	if u.Fields.empty() {
		return nil, errors.New("no fields given to update")
	}
	if u.Fields.Status != nil && !validStatuses[*u.Fields.Status] {
		return nil, &FieldError{Field: "status", Value: *u.Fields.Status, Allowed: statuses}
	}
	if u.Fields.Type != nil && !validItemTypes[*u.Fields.Type] {
		return nil, &FieldError{Field: "type", Value: *u.Fields.Type, Allowed: itemTypes}
	}
	if u.Fields.Label != nil && u.Fields.Labels != nil {
		return nil, errors.New("give label or labels, not both")
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var current struct {
		projectID, itemType, title, status, updatedAt string
		description, component, assignee              sql.NullString
		priority                                      sql.NullInt64
		deletedAt                                     sql.NullString
	}
	err = tx.QueryRowContext(ctx, `SELECT project_id, type, title, description, component,
		status, assignee, priority, updated_at, deleted_at FROM items WHERE id = ?`, id).
		Scan(&current.projectID, &current.itemType, &current.title, &current.description,
			&current.component, &current.status, &current.assignee, &current.priority, &current.updatedAt,
			&current.deletedAt)
	if err == sql.ErrNoRows {
		return nil, &lookupError{msg: fmt.Sprintf("item %q not found", id), kind: ErrNotFound}
	}
	if err != nil {
		return nil, fmt.Errorf("looking up item %q: %w", id, err)
	}
	if current.deletedAt.Valid {
		return nil, errDeleted("item", id)
	}
	if u.ExpectedUpdatedAt != "" && u.ExpectedUpdatedAt != current.updatedAt {
		// Hand back what is there now, so the caller can show the difference
		// rather than making the person fetch it again. Read it through this
		// transaction, not a fresh connection: it is the state this check was
		// made against, and it avoids opening a connection mid-transaction.
		item, getErr := scanItem(tx.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE id = ?`, id))
		if getErr != nil {
			return nil, getErr
		}
		return item, &ConflictError{EntityType: "item", EntityID: id,
			Expected: u.ExpectedUpdatedAt, Actual: current.updatedAt}
	}
	if u.Fields.Type != nil && *u.Fields.Type != current.itemType {
		if err := db.permit(ctx, OpChangeItemType, Target{
			ProjectID: current.projectID, EntityType: "item", EntityID: id,
		}); err != nil {
			return nil, err
		}
	}

	f := u.Fields
	var setClauses []string
	var args []any
	set := func(clause string, value any) {
		setClauses = append(setClauses, clause)
		args = append(args, value)
	}

	// Field edits collapse into one "updated" row, as they always have.
	fieldChanges := map[string][2]string{}
	if f.Title != nil && *f.Title != current.title {
		if current.itemType == "component" {
			if err := validateComponentTitleUnique(ctx, tx, current.projectID, *f.Title, id); err != nil {
				return nil, err
			}
		}
		fieldChanges["title"] = [2]string{current.title, *f.Title}
		set("title = ?", *f.Title)
	}
	if f.Description != nil && *f.Description != current.description.String {
		fieldChanges["description"] = [2]string{current.description.String, *f.Description}
		set("description = ?", nullIfEmpty(*f.Description))
	}
	// Label keeps its meaning: replace the item's whole label set with this one
	// value ("" clears all). The rows live in item_labels now.
	labelChanged := false
	if f.Label != nil || f.Labels != nil {
		var want []string
		if f.Label != nil && strings.TrimSpace(*f.Label) != "" {
			v, err := normalizeLabel(*f.Label)
			if err != nil {
				return nil, err
			}
			want = []string{v}
		}
		if f.Labels != nil {
			var err error
			if want, err = normalizeLabels(*f.Labels); err != nil {
				return nil, err
			}
		}
		changed, before, after, err := db.replaceItemLabelsTx(ctx, tx, id, want, nowUTC())
		if err != nil {
			return nil, err
		}
		if changed {
			labelChanged = true
			fieldChanges["label"] = [2]string{before, after}
		}
	}
	if f.Component != nil && *f.Component != current.component.String {
		if *f.Component != "" {
			if err := validateComponentTitle(ctx, tx, *f.Component); err != nil {
				return nil, err
			}
		}
		fieldChanges["component"] = [2]string{current.component.String, *f.Component}
		set("component = ?", nullIfEmpty(*f.Component))
	}

	// The rest keep their own audit operations, because history already reads
	// that way and a status change is a different kind of fact from a retitle.
	//
	// An entry is one audit row: either a from/to pair, or the map of field
	// changes that "updated" has always carried.
	type entry struct {
		operation string
		detail    map[string]string
		fields    map[string][2]string
	}
	var entries []entry
	if f.Status != nil && *f.Status != current.status {
		set("status = ?", *f.Status)
		entries = append(entries, entry{operation: "status_changed", detail: map[string]string{"from": current.status, "to": *f.Status}})
	}
	if f.Assignee != nil && *f.Assignee != current.assignee.String {
		set("assignee = ?", nullIfEmpty(*f.Assignee))
		entries = append(entries, entry{operation: "assigned", detail: map[string]string{"from": current.assignee.String, "to": *f.Assignee}})
	}
	if f.Priority != nil && (!current.priority.Valid || *f.Priority != current.priority.Int64) {
		set("priority = ?", *f.Priority)
		old := "(none)"
		if current.priority.Valid {
			old = fmt.Sprintf("%d", current.priority.Int64)
		}
		entries = append(entries, entry{operation: "priority_changed", detail: map[string]string{"from": old, "to": fmt.Sprintf("%d", *f.Priority)}})
	}
	if f.Type != nil && *f.Type != current.itemType {
		set("type = ?", *f.Type)
		entries = append(entries, entry{operation: "type_changed", detail: map[string]string{"from": current.itemType, "to": *f.Type}})
	}

	if len(fieldChanges) > 0 {
		entries = append(entries, entry{operation: "updated", fields: fieldChanges})
	}

	// Nothing actually differs: report the item as it is, and write nothing.
	if len(setClauses) == 0 && !labelChanged {
		return db.GetItem(ctx, id)
	}

	now := nowUTC()
	set("updated_at = ?", now)
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, "UPDATE items SET "+strings.Join(setClauses, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, fmt.Errorf("updating item: %w", err)
	}

	// One save, several kinds of change: tie the rows together so the audit
	// log shows one act rather than three coincidences.
	auditCtx := ctx
	if len(entries) > 1 && batchFromContext(ctx) == "" {
		auditCtx = WithBatch(ctx, uuid.NewString())
	}
	for _, e := range entries {
		var detail []byte
		if e.fields != nil {
			detail, _ = json.Marshal(e.fields)
		} else {
			detail, _ = json.Marshal(e.detail)
		}
		if err := db.insertAudit(auditCtx, tx, "item", id, e.operation, string(detail)); err != nil {
			return nil, fmt.Errorf("writing audit log: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetItem(ctx, id)
}
