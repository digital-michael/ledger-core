package ledgercore

// Vocabulary is the closed set of values the ledger accepts for each of its
// enumerated columns. It is the single source for three things that must
// never disagree:
//
//   - validation on write, in this package (validItemTypes etc. below);
//   - the database triggers that enforce the same sets at the storage layer,
//     against writers that bypass this package entirely (enforce.go);
//   - whatever a caller presents to a person as the valid choices
//     (ledger-server's /api/meta), so a UI can offer only valid values
//     instead of reporting invalid ones after the fact.
//
// Values are storage keys, not display labels: "in_progress", not
// "In progress". Presentation belongs to the caller.
type Vocabulary struct {
	ItemTypes     []string
	Statuses      []string
	RelationTypes []string
	NoteTypes     []string
}

// Note types. Only NoteTypeComment is writable through AddNote; the two timer
// types are written exclusively by StartTimer/StopTimer, which enforce the
// start/stop pairing that a free-form insert cannot. Timer events are history
// and are never editable (see UpdateNote).
const (
	NoteTypeComment     = "comment"
	NoteTypeTimeStarted = "time-started"
	NoteTypeTimeEnded   = "time-ended"
)

// The ordered sets. Order is meaningful for presentation -- statuses run in
// lifecycle order -- and is also the order used in error messages.
var (
	// itemTypes covers the full work lifecycle: discovery (spike), planning
	// (epic, story, plan), implementation (task), quality (defect),
	// deployment (release), and support (incident) -- plus component, a
	// structural/specification marker orthogonal to the rest (see Item's doc
	// comment).
	itemTypes = []string{"epic", "story", "task", "spike", "plan", "defect", "release", "incident", "component"}

	// statuses is a real input to CreateItem, not always hardcoded: creating
	// an item already done (or in_progress) is a common need, e.g. logging
	// past work.
	statuses = []string{"backlog", "planned", "in_progress", "blocked", "done"}

	// relationTypes used to be enforced only by the MCP tool layer's
	// mcp.Enum, on the reasoning that "the tool layer is where user-facing
	// validation belongs". That did not hold: mcp.Enum populates the
	// advertised JSON schema but does not reject at runtime, so four rows
	// outside this set reached the database -- blocks (x2), follows_up_on,
	// and serves -- two of them written 10 and 11 days after the enum
	// shipped. A declared vocabulary nothing enforces is documentation, not a
	// constraint. This package is the right home because it is the one layer
	// every writer passes through; a tool layer is only one of several front
	// doors.
	relationTypes = []string{"blocked_by", "depends_on", "related_to", "part_of"}

	noteTypes = []string{NoteTypeComment, NoteTypeTimeStarted, NoteTypeTimeEnded}
)

// Derived lookup sets. Derived, never hand-written, so they cannot drift from
// the ordered sets above. The "must be one of ..." wording that used to live
// here is now built by FieldError from the same slices.
var (
	validItemTypes     = setOf(itemTypes)
	validStatuses      = setOf(statuses)
	validRelationTypes = setOf(relationTypes)
	validNoteTypes     = setOf(noteTypes)

)

// Vocab returns the ledger's vocabulary. Each call returns fresh slices, so
// a caller cannot mutate the package's own sets.
func Vocab() Vocabulary {
	return Vocabulary{
		ItemTypes:     append([]string(nil), itemTypes...),
		Statuses:      append([]string(nil), statuses...),
		RelationTypes: append([]string(nil), relationTypes...),
		NoteTypes:     append([]string(nil), noteTypes...),
	}
}

func setOf(values []string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}
