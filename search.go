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
	like := likePattern(query)
	var results []SearchResult

	// SQLite decides which field matched (title_match), rather than a Go
	// substring test: with wildcards in the pattern the two would disagree.
	itemQuery := `SELECT id, title, type, status, (title LIKE ? ESCAPE '\') FROM items
		WHERE deleted_at IS NULL AND (title LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\')`
	itemArgs := []any{like, like, like}
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
		var titleMatch bool
		if err := itemRows.Scan(&id, &title, &typ, &status, &titleMatch); err != nil {
			itemRows.Close()
			return nil, err
		}
		matchedIn := "description"
		if titleMatch {
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
		WHERE notes.deleted_at IS NULL AND items.deleted_at IS NULL AND notes.body LIKE ? ESCAPE '\'`
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

// likePattern turns what a person typed into a SQL LIKE pattern.
//
//   - the search is "contains", so the pattern is wrapped in % either way;
//   - '*' and '?' are the wildcards people actually type;
//   - '%' and '_' are literal. SQLite's LIKE would treat them as wildcards,
//     which meant a search for "50%" quietly matched everything after "50".
//
// Used with ESCAPE '\' by every query that searches user text.
func likePattern(query string) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, r := range query {
		switch r {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('%')
	return b.String()
}
