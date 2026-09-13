package ledgercore

import "context"

// Who performed a change.
//
// The ledger recorded only a program name until 2026-09-12 ("claude-code",
// "mcp-console/run"), which cannot answer "who wrote this comment" and cannot
// tell two concurrent sessions of the same program apart. With several people
// and several agents writing, that is the difference between a history and a
// rumour.
//
// An actor is deliberately not a user account: there is no login yet, and
// inventing one would mean recording a person's identity the ledger cannot
// actually establish. Today every caller is a program and says so. When a
// login exists, ledger-server starts declaring ActorPerson with a real id,
// and nothing else has to change.
type ActorKind string

const (
	ActorPerson  ActorKind = "person"  // a human, established by something that can prove it
	ActorAgent   ActorKind = "agent"   // an autonomous agent acting on someone's behalf
	ActorProgram ActorKind = "program" // a program, with no claim about who is driving it
)

// Actor identifies the writer. ID is stable and machine-readable; Label is
// what a person reads.
type Actor struct {
	Kind  ActorKind
	ID    string
	Label string
}

// String is the canonical stored form, "kind:id" -- empty when unknown, which
// is stored as NULL rather than as a guess.
func (a Actor) String() string {
	if a.ID == "" {
		return ""
	}
	kind := a.Kind
	if kind == "" {
		kind = ActorProgram
	}
	return string(kind) + ":" + a.ID
}

// DisplayName is Label, falling back to ID.
func (a Actor) DisplayName() string {
	if a.Label != "" {
		return a.Label
	}
	return a.ID
}

// currentActor asks the caller who is writing. A caller that supplies no
// resolver records nothing rather than something invented.
func (db *DB) currentActor(ctx context.Context) Actor {
	if db.actor == nil {
		return Actor{}
	}
	return db.actor(ctx)
}

// batchKey carries the id that ties one bulk action's audit rows together.
type batchKey struct{}

// WithBatch marks ctx as one action. Every audit row written under it shares a
// batch id, so a bulk change reads as a single event with N parts instead of N
// unrelated rows -- which is what it always was in the data until now.
//
// Bulk operations stay per-item rather than all-or-nothing: partial success is
// reported honestly, and the batch id is what makes the partial result
// reconstructable afterwards.
func WithBatch(ctx context.Context, batchID string) context.Context {
	return context.WithValue(ctx, batchKey{}, batchID)
}

func batchFromContext(ctx context.Context) string {
	id, _ := ctx.Value(batchKey{}).(string)
	return id
}
