package ledger

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// ItemRelation is a cross-cutting edge between two items that isn't
// containment (that's ParentID on Item instead) — e.g. a story depending on
// a task under a different epic. No cycle detection: tolerated, not
// defended against.
type ItemRelation struct {
	ID           string
	FromItemID   string
	ToItemID     string
	RelationType string
	CreatedAt    string
}

// validRelationTypesDesc lists the allowed values for RelateItems' and
// BulkRelateItems' relationType, for use in rejection error messages.
const validRelationTypesDesc = "blocked_by, depends_on, related_to, part_of"

// validRelationTypes mirrors validStatuses'/validItemTypes' shape in items.go.
//
// This validation used to live only in the tool layer's mcp.Enum, on the
// stated reasoning that "the tool layer is where user-facing validation
// belongs". That turned out not to hold: mcp.Enum populates the advertised
// JSON schema but does not reject at runtime, so four rows outside this set
// reached the database -- blocks (x2), follows_up_on, and serves -- two of
// them written 10 and 11 days AFTER the enum shipped. A declared vocabulary
// nothing enforces is documentation, not a constraint.
//
// The store is the right home for it because it is the one layer every
// writer passes through; the tool layer is only one of several front doors
// (see also the planned ledger-server HTTP API, epic d28ed3b2). The tool
// layer keeps its enum for discoverability -- it is what tells a caller the
// vocabulary exists -- but is no longer the thing relied on to enforce it.
var validRelationTypes = map[string]bool{
	"blocked_by": true, "depends_on": true, "related_to": true, "part_of": true,
}

// RelateItems inserts a relation between two items, rejecting any
// relationType outside validRelationTypes.
func (db *DB) RelateItems(ctx context.Context, fromID, toID, relationType string) (*ItemRelation, error) {
	if !validRelationTypes[relationType] {
		return nil, fmt.Errorf("invalid relation_type %q: must be one of %s", relationType, validRelationTypesDesc)
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	id := uuid.NewString()
	now := nowUTC()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO item_relations (id, from_item_id, to_item_id, relation_type, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, fromID, toID, relationType, now,
	)
	if err != nil {
		return nil, fmt.Errorf("inserting item relation: %w", err)
	}

	detail, _ := json.Marshal(map[string]string{"from_item_id": fromID, "to_item_id": toID, "relation_type": relationType})
	if err := insertAudit(ctx, tx, "item_relation", id, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &ItemRelation{ID: id, FromItemID: fromID, ToItemID: toID, RelationType: relationType, CreatedAt: now}, nil
}

// BulkRelateResult is one from-id's outcome from BulkRelateItems.
type BulkRelateResult struct {
	FromID   string
	Relation *ItemRelation
	Error    error
}

// BulkRelateItems relates each of fromIDs to toID independently -- one bad id
// (including one that doesn't exist, rejected by item_relations' foreign key
// constraint) doesn't block the rest. Each success gets its own real
// audit_log entry, exactly as if related individually via RelateItems.
func (db *DB) BulkRelateItems(ctx context.Context, fromIDs []string, toID, relationType string) []BulkRelateResult {
	results := make([]BulkRelateResult, 0, len(fromIDs))
	for _, fromID := range fromIDs {
		rel, err := db.RelateItems(ctx, fromID, toID, relationType)
		results = append(results, BulkRelateResult{FromID: fromID, Relation: rel, Error: err})
	}
	return results
}

// ListRelations returns every relation touching itemID, in either direction
// (it's the from side or the to side) — "what does this item depend on" and
// "what depends on this item" are both real questions, so both directions
// are returned rather than requiring two separate calls.
func (db *DB) ListRelations(ctx context.Context, itemID string) ([]ItemRelation, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, from_item_id, to_item_id, relation_type, created_at
		 FROM item_relations WHERE (from_item_id = ? OR to_item_id = ?) AND deleted_at IS NULL
		 ORDER BY created_at`, itemID, itemID)
	if err != nil {
		return nil, fmt.Errorf("listing relations: %w", err)
	}
	defer rows.Close()

	var relations []ItemRelation
	for rows.Next() {
		var r ItemRelation
		if err := rows.Scan(&r.ID, &r.FromItemID, &r.ToItemID, &r.RelationType, &r.CreatedAt); err != nil {
			return nil, err
		}
		relations = append(relations, r)
	}
	return relations, rows.Err()
}

// ListProjectRelations returns every relation where either side belongs to
// projectID -- the whole cross-cutting dependency graph touching a project,
// not just one item's neighbors. item_relations has no project_id column of
// its own, and relations can cross projects (nothing prevents it), so this
// joins to items on BOTH from_item_id and to_item_id, matching if either
// side belongs to projectID.
func (db *DB) ListProjectRelations(ctx context.Context, projectID string) ([]ItemRelation, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT r.id, r.from_item_id, r.to_item_id, r.relation_type, r.created_at
		 FROM item_relations r
		 JOIN items i_from ON r.from_item_id = i_from.id
		 JOIN items i_to ON r.to_item_id = i_to.id
		 WHERE (i_from.project_id = ? OR i_to.project_id = ?) AND r.deleted_at IS NULL
		 ORDER BY r.created_at`, projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing project relations: %w", err)
	}
	defer rows.Close()

	var relations []ItemRelation
	for rows.Next() {
		var r ItemRelation
		if err := rows.Scan(&r.ID, &r.FromItemID, &r.ToItemID, &r.RelationType, &r.CreatedAt); err != nil {
			return nil, err
		}
		relations = append(relations, r)
	}
	return relations, rows.Err()
}
