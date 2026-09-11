package ledgercore

import "errors"

// Lookup failures a caller may need to tell apart -- an HTTP server answers
// 404 for one and 409 for the other. Match with errors.Is; the messages
// themselves are unchanged, because mcp-local prints them to agents.
//
// Deliberately only these two for now. Typed field-level validation errors
// are planned for ledger-server Phase 2, the first phase that writes.
var (
	ErrNotFound    = errors.New("not found")
	ErrAmbiguousID = errors.New("ambiguous id")
)

// lookupError carries its own message and unwraps to its kind.
type lookupError struct {
	msg  string
	kind error
}

func (e *lookupError) Error() string { return e.msg }
func (e *lookupError) Unwrap() error { return e.kind }
