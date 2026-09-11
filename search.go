package ledgercore

import (
	"context"
	"database/sql"
	"strings"
)

// SearchResult is one match from SearchItems -- MatchedIn names which field
// matched ("title", "description", or "note") so a caller can tell an
// item-level match from a note-level one; Snippet carries the matching
// note's body for note matches, empty otherwise.
type SearchResult struct {
	ItemID    string
	Title     string
	Type      string
	Status    string
	MatchedIn string
	Snippet   string
}

// SearchItems does a substring, case-insensitive search across item titles,
// item descriptions, and item-attached note bodies -- ledger_find only ever
// covered titles, leaving the substantial root-cause/verification detail
// this project routinely puts in descriptions and notes unsearchable except
// by listing everything and eyeballing it. Same "no FTS/index, plain LIKE
// scan" philosophy as FindItems -- fine at this ledger's actual scale, not
// backed by a new dependency. Does not search standalone project-level
// notes (item_id IS NULL) -- those have no item to report as the match,
// out of scope for this pass.
func (db *DB) SearchItems(ctx context.Context, projectID, query string) ([]SearchResult, error) {
	like := "%" + query + "%"
	lowerQuery := strings.ToLower(query)
	var results []SearchResult

	itemQuery := `SELECT id, title, type, status, description FROM items
		WHERE deleted_at IS NULL AND (title LIKE ? OR description LIKE ?)`
	itemArgs := []any{like, like}
	if projectID != "" {
		itemQuery += ` AND project_id = ?`
		itemArgs = append(itemArgs, projectID)
	}
	itemQuery += ` ORDER BY created_at`

	itemRows, err := db.conn.QueryContext(ctx, itemQuery, itemArgs...)
	if err != nil {
		return nil, err
	}
	for itemRows.Next() {
		var id, title, typ, status string
		var desc sql.NullString
		if err := itemRows.Scan(&id, &title, &typ, &status, &desc); err != nil {
			itemRows.Close()
			return nil, err
		}
		matchedIn := "description"
		if strings.Contains(strings.ToLower(title), lowerQuery) {
			matchedIn = "title"
		}
		results = append(results, SearchResult{ItemID: id, Title: title, Type: typ, Status: status, MatchedIn: matchedIn})
	}
	if err := itemRows.Err(); err != nil {
		itemRows.Close()
		return nil, err
	}
	itemRows.Close()

	noteQuery := `SELECT items.id, items.title, items.type, items.status, notes.body
		FROM notes JOIN items ON notes.item_id = items.id
		WHERE notes.deleted_at IS NULL AND items.deleted_at IS NULL AND notes.body LIKE ?`
	noteArgs := []any{like}
	if projectID != "" {
		noteQuery += ` AND items.project_id = ?`
		noteArgs = append(noteArgs, projectID)
	}
	noteQuery += ` ORDER BY notes.created_at`

	noteRows, err := db.conn.QueryContext(ctx, noteQuery, noteArgs...)
	if err != nil {
		return nil, err
	}
	defer noteRows.Close()
	for noteRows.Next() {
		var id, title, typ, status string
		var body sql.NullString
		if err := noteRows.Scan(&id, &title, &typ, &status, &body); err != nil {
			return nil, err
		}
		results = append(results, SearchResult{
			ItemID: id, Title: title, Type: typ, Status: status,
			MatchedIn: "note", Snippet: body.String,
		})
	}
	if err := noteRows.Err(); err != nil {
		return nil, err
	}

	return results, nil
}
