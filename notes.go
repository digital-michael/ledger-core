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

// Note is a single row in the flexible event/comment log: either a
// freeform comment (type="comment") or a paired timer event
// (type="time-started"/"time-ended"). Elapsed time and "is running" are
// derived by pairing consecutive timer-typed rows on read, not stored.
type Note struct {
	ID        string
	ProjectID sql.NullString
	ItemID    sql.NullString
	Type      string
	Body      sql.NullString
	URL       sql.NullString
	CreatedAt string
	UpdatedAt string
}

// AddNoteParams are the inputs to inserting a note. Type defaults to
// "comment"; ProjectID and/or ItemID may be empty for a standalone/global
// note (both empty), a project-level note (ItemID empty), or an
// item-attached note (both set, or just ItemID).
type AddNoteParams struct {
	ProjectID string
	ItemID    string
	Type      string
	Body      string
	URL       string
}

// AddNote inserts a freeform comment. Type may be empty (meaning comment) or
// NoteTypeComment; anything else is rejected.
//
// Timer events are written only via StartTimer/StopTimer, which enforce the
// start/stop pairing that a free-form insert cannot. AddNote used to store
// whatever Type it was given, so a caller could write a "time-started" note
// here and fabricate unpaired timer history. Unreachable through the MCP tool
// (which never exposed a type parameter), but reachable by any program
// importing this package directly -- found and closed 2026-09-10 during the
// ledger-core extraction.
func (db *DB) AddNote(ctx context.Context, p AddNoteParams) (*Note, error) {
	if p.Type == "" {
		p.Type = NoteTypeComment
	}
	if p.Type != NoteTypeComment {
		if validNoteTypes[p.Type] {
			return nil, fmt.Errorf("note type %q can't be written with AddNote: timer events are recorded only by StartTimer/StopTimer", p.Type)
		}
		return nil, fmt.Errorf("invalid note type %q: AddNote writes only %q", p.Type, NoteTypeComment)
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	note, err := db.insertNote(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return note, nil
}

// insertNote is the shared insert path used by AddNote, StartTimer, and
// StopTimer — the one place a notes row and its audit_log entry are
// written, in the caller's transaction.
func (db *DB) insertNote(ctx context.Context, tx *sql.Tx, p AddNoteParams) (*Note, error) {
	id := uuid.NewString()
	now := nowUTC()
	_, err := tx.ExecContext(ctx,
		`INSERT INTO notes (id, project_id, item_id, type, body, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, nullIfEmpty(p.ProjectID), nullIfEmpty(p.ItemID), p.Type, nullIfEmpty(p.Body), nullIfEmpty(p.URL), now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("inserting note: %w", err)
	}
	// Identifying fields only, matching item creation (title and type, not
	// the description). This was written with an empty detail until
	// 2026-09-10 -- the same gap the 2026-07-30 fix closed for resources and
	// relations but missed for notes -- which left a comment, a genuine timer
	// event and a fabricated one indistinguishable in the audit trail.
	createdDetail := map[string]string{"type": p.Type}
	if p.ItemID != "" {
		createdDetail["item_id"] = p.ItemID
	}
	if p.ProjectID != "" {
		createdDetail["project_id"] = p.ProjectID
	}
	detail, _ := json.Marshal(createdDetail)
	if err := db.insertAudit(ctx, tx, "note", id, "created", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}
	return &Note{
		ID:        id,
		ProjectID: sql.NullString{String: p.ProjectID, Valid: p.ProjectID != ""},
		ItemID:    sql.NullString{String: p.ItemID, Valid: p.ItemID != ""},
		Type:      p.Type,
		Body:      sql.NullString{String: p.Body, Valid: p.Body != ""},
		URL:       sql.NullString{String: p.URL, Valid: p.URL != ""},
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// ListNotes returns notes for an item (its full "Comment History", timer
// events included) or, if itemID is empty, standalone notes attached to a
// project. Exactly one of itemID/projectID must be non-empty.
func (db *DB) ListNotes(ctx context.Context, itemID, projectID string) ([]Note, error) {
	var rows *sql.Rows
	var err error
	switch {
	case itemID != "":
		rows, err = db.conn.QueryContext(ctx,
			`SELECT id, project_id, item_id, type, body, url, created_at, updated_at
			 FROM notes WHERE item_id = ? AND deleted_at IS NULL ORDER BY created_at`, itemID)
	case projectID != "":
		rows, err = db.conn.QueryContext(ctx,
			`SELECT id, project_id, item_id, type, body, url, created_at, updated_at
			 FROM notes WHERE project_id = ? AND item_id IS NULL AND deleted_at IS NULL ORDER BY created_at`, projectID)
	default:
		return nil, errors.New("ledger_list_notes requires an item id or a project id")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.ProjectID, &n.ItemID, &n.Type, &n.Body, &n.URL, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

// StartTimer records a time-started event for item. Rejects a second start
// with no intervening stop — the pairing invariant lives here, not in the
// caller.
func (db *DB) StartTimer(ctx context.Context, itemID string) (*Note, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	open, err := timerIsOpen(ctx, tx, itemID)
	if err != nil {
		return nil, err
	}
	if open {
		return nil, fmt.Errorf("timer already running for item %q; stop it before starting again", itemID)
	}

	note, err := db.insertNote(ctx, tx, AddNoteParams{ItemID: itemID, Type: NoteTypeTimeStarted})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return note, nil
}

// StopTimer records a time-ended event for item. Rejects a stop with no
// open start.
func (db *DB) StopTimer(ctx context.Context, itemID string) (*Note, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	open, err := timerIsOpen(ctx, tx, itemID)
	if err != nil {
		return nil, err
	}
	if !open {
		return nil, fmt.Errorf("no running timer for item %q", itemID)
	}

	note, err := db.insertNote(ctx, tx, AddNoteParams{ItemID: itemID, Type: NoteTypeTimeEnded})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return note, nil
}

// timerIsOpen reports whether item's most recent timer-typed note is an
// unmatched time-started.
func timerIsOpen(ctx context.Context, tx *sql.Tx, itemID string) (bool, error) {
	var noteType string
	err := tx.QueryRowContext(ctx,
		`SELECT type FROM notes WHERE item_id = ? AND type IN ('time-started','time-ended') AND deleted_at IS NULL
		 ORDER BY created_at DESC LIMIT 1`,
		itemID,
	).Scan(&noteType)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return noteType == NoteTypeTimeStarted, nil
}

// UpdateNoteParams carries the fields UpdateNote may change. Same convention
// as UpdateItemParams: a nil pointer leaves the field unchanged, a pointer to
// "" clears it.
type UpdateNoteParams struct {
	Body *string
	URL  *string
}

// UpdateNote edits a comment's body and/or URL, recording the change in
// audit_log in the same transaction. Only fields that are both given and
// actually different are written; if nothing differs, the current note is
// returned and no audit row is written, matching UpdateItem.
//
// Only comments are editable. Timer events (NoteTypeTimeStarted/TimeEnded)
// are history: they are what elapsed time is computed from, so editing one
// would silently falsify recorded time. They are refused, not ignored.
//
// A soft-deleted note is refused rather than edited: restore it first.
func (db *DB) UpdateNote(ctx context.Context, id string, p UpdateNoteParams) (*Note, error) {
	if p.Body == nil && p.URL == nil {
		return nil, errors.New("at least one of body or url must be given")
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var noteType string
	var oldBody, oldURL, deletedAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT type, body, url, deleted_at FROM notes WHERE id = ?`, id).
		Scan(&noteType, &oldBody, &oldURL, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("note %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up note %q: %w", id, err)
	}
	if noteType != NoteTypeComment {
		return nil, fmt.Errorf("note %q is a %s timer event, which is history and cannot be edited; only comments can be", id, noteType)
	}
	if deletedAt.Valid {
		return nil, fmt.Errorf("note %q is deleted; restore it first (ledger_restore entity_type=note id=%s)", id, id)
	}

	changes := map[string][2]string{}
	var setClauses []string
	var args []any
	if p.Body != nil && *p.Body != oldBody.String {
		changes["body"] = [2]string{oldBody.String, *p.Body}
		setClauses = append(setClauses, "body = ?")
		args = append(args, nullIfEmpty(*p.Body))
	}
	if p.URL != nil && *p.URL != oldURL.String {
		changes["url"] = [2]string{oldURL.String, *p.URL}
		setClauses = append(setClauses, "url = ?")
		args = append(args, nullIfEmpty(*p.URL))
	}
	if len(setClauses) == 0 {
		return getNote(ctx, tx, id)
	}

	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, nowUTC(), id)
	if _, err := tx.ExecContext(ctx, "UPDATE notes SET "+strings.Join(setClauses, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, fmt.Errorf("updating note: %w", err)
	}

	detail, _ := json.Marshal(changes)
	if err := db.insertAudit(ctx, tx, "note", id, "updated", string(detail)); err != nil {
		return nil, fmt.Errorf("writing audit log: %w", err)
	}

	n, err := getNote(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return n, nil
}

// getNote reads one note by exact id within tx.
func getNote(ctx context.Context, tx *sql.Tx, id string) (*Note, error) {
	var n Note
	err := tx.QueryRowContext(ctx,
		`SELECT id, project_id, item_id, type, body, url, created_at, updated_at FROM notes WHERE id = ?`, id).
		Scan(&n.ID, &n.ProjectID, &n.ItemID, &n.Type, &n.Body, &n.URL, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("reading note %q: %w", id, err)
	}
	return &n, nil
}
