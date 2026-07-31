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

// RelateItems inserts a relation between two items. relationType is one of
// blocked_by, depends_on, related_to — not validated against that set here;
// the tool layer is where user-facing validation belongs.
func (db *DB) RelateItems(ctx context.Context, fromID, toID, relationType string) (*ItemRelation, error) {
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
