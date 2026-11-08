package ledgercore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Who made a ticket and who is accountable for it (ledger story a94140e8).
//
//   - created_by is who actually wrote it: the writer's actor string, set once at
//     creation and never changed by any operation. Unknown is NULL, not a guess.
//   - owner is the accountable person. It changes only through TransferItemOwner,
//     an audited operation of its own -- not through a field edit, because moving
//     accountability should be a deliberate act that leaves its own trace.

// defaultOwnerTx picks the owner of a new item when none was given: the person
// who created it; failing that, the project's first owner; failing that nobody.
// An agent that creates a ticket is therefore not made accountable for it.
func defaultOwnerTx(ctx context.Context, tx *sql.Tx, projectID string, creator Actor) (any, error) {
	if s := creator.String(); s != "" {
		if id, err := ParseIdentity(FieldOwner, s); err == nil {
			return id, nil
		}
	}
	var owner string
	err := tx.QueryRowContext(ctx,
		`SELECT member FROM project_members WHERE project_id = ? AND role = 'owner' AND deleted_at IS NULL ORDER BY created_at, id LIMIT 1`,
		projectID).Scan(&owner)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return owner, nil
}

// TransferItemOwner makes newOwner the item's accountable person. It must be a
// person (identity.go). The same owner again writes nothing. A deleted item
// refuses; the policy may refuse too (OpTransferOwner).
func (db *DB) TransferItemOwner(ctx context.Context, itemID, newOwner string) (*Item, error) {
	owner, err := ParseIdentity(FieldOwner, newOwner)
	if err != nil {
		return nil, err
	}
	id, err := db.resolveItemID(ctx, itemID)
	if err != nil {
		return nil, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := refuseIfItemDeleted(ctx, tx, id); err != nil {
		return nil, err
	}
	var projectID string
	var current sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT project_id, owner FROM items WHERE id = ?`, id).Scan(&projectID, &current); err != nil {
		return nil, err
	}
	if err := db.permit(ctx, OpTransferOwner, Target{ProjectID: projectID, EntityType: "item", EntityID: id}); err != nil {
		return nil, err
	}
	if current.Valid && current.String == owner {
		return db.GetItem(ctx, id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE items SET owner = ?, updated_at = ? WHERE id = ?`, owner, nowUTC(), id); err != nil {
		return nil, fmt.Errorf("transferring ownership: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"from": current.String, "to": owner})
	if err := db.insertAudit(ctx, tx, "item", id, "owner_changed", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetItem(ctx, id)
}

// ProjectCreatedBy returns who created a project ("" when unknown).
func (db *DB) ProjectCreatedBy(ctx context.Context, projectID string) (string, error) {
	var by sql.NullString
	err := db.conn.QueryRowContext(ctx, `SELECT created_by FROM projects WHERE id = ?`, projectID).Scan(&by)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("project %q not found", projectID)
	}
	return by.String, err
}

// ---- backfill ----

// CreatorFromAudit says who an old record's creator was, from the first
// "created" audit row of that item or project. Old writers recorded a client
// name and, later, an actor; neither said who was driving. The mapping
// (decided 2026-11-07):
//
//   - an actor, when there is one, is kept -- except that a Claude Code session
//     recorded as a program is an agent: program:claude-code -> agent:claude-code;
//   - with no actor, the client name decides: claude-code -> agent:claude-code;
//     mcp-console/... -> program:mcp-console (a program with no claim about its
//     driver); the web UI -> program:ledger-server; mcp -> program:mcp;
//   - with no record at all, fallback (the workstation's person).
//
// source says which of the three it came from, for the report.
func CreatorFromAudit(actor, client, fallback string) (identity, source string) {
	if actor != "" {
		if actor == "program:claude-code" {
			return "agent:claude-code", "audit actor"
		}
		return actor, "audit actor"
	}
	switch {
	case client == "claude-code":
		return "agent:claude-code", "audit client"
	case strings.HasPrefix(client, "mcp-console"):
		return "program:mcp-console", "audit client"
	case client == "the ledger web UI":
		return "program:ledger-server", "audit client"
	case client == "mcp":
		return "program:mcp", "audit client"
	}
	return fallback, "no record"
}

// BackfillOptions configures BackfillPeople.
type BackfillOptions struct {
	Owner string // the person who owns everything that has no owner yet (a person identity)
	Apply bool   // false: count what would change and change nothing
}

// BackfillReport counts what a backfill changed (or, in a dry run, would).
type BackfillReport struct {
	Items       int            // items given a created_by and/or owner
	Projects    int            // projects given a created_by
	OwnersAdded int            // projects given their first owner member
	BySource    map[string]int // where each created_by came from
	ByCreator   map[string]int // how many of each identity
	Applied     bool
}

// BackfillPeople fills created_by and owner on existing items and projects, and
// gives every project without an owner the Owner person as its first owner.
// It only ever fills what is empty: a value already set is never overwritten, so
// it is safe to run again and writes nothing the second time. One transaction,
// one audit batch; a dry run rolls back.
func (db *DB) BackfillPeople(ctx context.Context, o BackfillOptions) (*BackfillReport, error) {
	owner, err := ParseIdentity(FieldOwner, o.Owner)
	if err != nil {
		return nil, err
	}
	ctx = WithBatch(ctx, uuid.NewString())
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rep := &BackfillReport{BySource: map[string]int{}, ByCreator: map[string]int{}, Applied: o.Apply}

	creatorOf := func(entityType, id string) (string, string, error) {
		var actor, client sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT actor, client FROM audit_log WHERE entity_type = ? AND entity_id = ? AND operation = 'created' ORDER BY created_at LIMIT 1`,
			entityType, id).Scan(&actor, &client)
		if err != nil && err != sql.ErrNoRows {
			return "", "", err
		}
		who, src := CreatorFromAudit(actor.String, client.String, owner)
		return who, src, nil
	}

	// Items.
	type itemRow struct {
		id        string
		createdBy sql.NullString
		owner     sql.NullString
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, created_by, owner FROM items WHERE created_by IS NULL OR owner IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	var items []itemRow
	for rows.Next() {
		var r itemRow
		if err := rows.Scan(&r.id, &r.createdBy, &r.owner); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, r := range items {
		d := map[string]string{"backfill": "people"}
		if !r.createdBy.Valid {
			who, src, err := creatorOf("item", r.id)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE items SET created_by = ? WHERE id = ?`, who, r.id); err != nil {
				return nil, err
			}
			d["created_by"] = who
			rep.BySource[src]++
			rep.ByCreator[who]++
		}
		if !r.owner.Valid {
			if _, err := tx.ExecContext(ctx, `UPDATE items SET owner = ? WHERE id = ?`, owner, r.id); err != nil {
				return nil, err
			}
			d["owner"] = owner
		}
		detail, _ := json.Marshal(d)
		if err := db.insertAudit(ctx, tx, "item", r.id, "updated", string(detail)); err != nil {
			return nil, err
		}
		rep.Items++
	}

	// Projects.
	prows, err := tx.QueryContext(ctx, `SELECT id, created_by IS NULL FROM projects ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	type projRow struct {
		id      string
		needsBy bool
	}
	var projs []projRow
	for prows.Next() {
		var p projRow
		if err := prows.Scan(&p.id, &p.needsBy); err != nil {
			prows.Close()
			return nil, err
		}
		projs = append(projs, p)
	}
	if err := prows.Err(); err != nil {
		prows.Close()
		return nil, err
	}
	prows.Close()
	now := nowUTC()
	for _, p := range projs {
		if p.needsBy {
			who, src, err := creatorOf("project", p.id)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET created_by = ? WHERE id = ?`, who, p.id); err != nil {
				return nil, err
			}
			detail, _ := json.Marshal(map[string]string{"backfill": "people", "created_by": who})
			if err := db.insertAudit(ctx, tx, "project", p.id, "updated", string(detail)); err != nil {
				return nil, err
			}
			rep.BySource[src]++
			rep.ByCreator[who]++
			rep.Projects++
		}
		n, err := liveOwnerCount(ctx, tx, p.id, "")
		if err != nil {
			return nil, err
		}
		if n == 0 {
			if _, _, err := db.addMemberTx(ctx, tx, p.id, owner, "owner", now); err != nil {
				return nil, err
			}
			rep.OwnersAdded++
		}
	}

	if !o.Apply {
		return rep, nil // the deferred Rollback leaves everything as it was
	}
	return rep, tx.Commit()
}
