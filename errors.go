package ledgercore

import (
	"errors"
	"fmt"
	"strings"
)

// Lookup failures a caller may need to tell apart -- an HTTP server answers
// 404 for one and 409 for the other. Match with errors.Is; the messages
// themselves are unchanged, because mcp-local prints them to agents.
//
// Deliberately only these two for now. Typed field-level validation errors
// are planned for ledger-server Phase 2, the first phase that writes.
var (
	ErrNotFound    = errors.New("not found")
	ErrAmbiguousID = errors.New("ambiguous id")

	// ErrInvalidField: a value outside what the field accepts. Match with
	// errors.Is, then errors.As for a *FieldError to learn which field.
	ErrInvalidField = errors.New("invalid field value")

	// ErrConflict: someone else changed it since it was loaded. Never
	// resolved silently -- the caller decides (docs/ledger.md U14).
	ErrConflict = errors.New("changed since it was loaded")
)

// lookupError carries its own message and unwraps to its kind.
type lookupError struct {
	msg  string
	kind error
}

func (e *lookupError) Error() string { return e.msg }
func (e *lookupError) Unwrap() error { return e.kind }

// FieldError says which field was wrong and what it would have accepted, so a
// caller can put the message beside the offending input rather than dropping a
// banner on the page. The text is unchanged from when these were plain
// fmt.Errorf calls, because mcp-local prints it to agents.
type FieldError struct {
	Field   string   // "status", "type", "relation_type"
	Value   string   // what was given
	Allowed []string // what would have been accepted, in the ledger's own order
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("invalid %s %q: must be one of %s", e.Field, e.Value, strings.Join(e.Allowed, ", "))
}

func (e *FieldError) Unwrap() error { return ErrInvalidField }

// ConflictError reports that the thing being edited moved under the editor's
// feet. It carries both timestamps so a caller can say what happened, and the
// current state travels back alongside it so the caller can show it without a
// second read.
type ConflictError struct {
	EntityType string
	EntityID   string
	Expected   string // the version the editor loaded
	Actual     string // what is there now
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s %q changed at %s, after the version you loaded (%s)",
		e.EntityType, e.EntityID, e.Actual, e.Expected)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }
