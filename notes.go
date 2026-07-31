package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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

// AddNote inserts a freeform comment. Timer events are written via
// StartTimer/StopTimer instead, which enforce the pairing invariant that
// AddNote does not.
func (db *DB) AddNote(ctx context.Context, p AddNoteParams) (*Note, error) {
	if p.Type == "" {
		p.Type = "comment"
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	note, err := insertNote(ctx, tx, p)
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
func insertNote(ctx context.Context, tx *sql.Tx, p AddNoteParams) (*Note, error) {
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
	if err := insertAudit(ctx, tx, "note", id, "created", ""); err != nil {
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

	note, err := insertNote(ctx, tx, AddNoteParams{ItemID: itemID, Type: "time-started"})
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

	note, err := insertNote(ctx, tx, AddNoteParams{ItemID: itemID, Type: "time-ended"})
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
	return noteType == "time-started", nil
}
