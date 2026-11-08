package ledgercore

import (
	"fmt"
	"strings"
)

// Identity format for every people field (decided 2026-11-07, ledger-server
// ownership story a94140e8).
//
// A person, agent or program is written as the "kind:id" string that
// audit_log.actor and notes.author already hold -- "person:digital-michael",
// "agent:claude-code". created_by, owner, assignees, watchers and project
// members all use it, so one value means the same thing in every column and a
// join between them needs no translation.
//
// This file only validates shape. It does not know who exists: locally there
// is no user registry, and a remote ledger checks a person's id against its
// identity provider. Agents are one kind among many ("agent:claude-code" is
// one of several), so no agent name is special-cased anywhere.

// Kinds a people field may hold. The three ActorKind values are the whole
// closed set; a fourth is a schema decision, not a value to pass anyway (U4).
var identityKinds = []string{string(ActorPerson), string(ActorAgent), string(ActorProgram)}

// IdentityField names a people field, for the kind rules and error messages.
type IdentityField string

const (
	FieldCreatedBy IdentityField = "created_by"
	FieldOwner     IdentityField = "owner"
	FieldAssignee  IdentityField = "assignee"
	FieldWatcher   IdentityField = "watcher"
	FieldMember    IdentityField = "member"
)

// fieldKinds says which kinds may appear in which field.
//
//   - created_by is who actually wrote it, so any kind, including a program.
//   - owner is the accountable party, always a person.
//   - assignee is who is doing the work; an agent may.
//   - watcher and member are people or agents; a bare program has no stake.
var fieldKinds = map[IdentityField][]string{
	FieldCreatedBy: {"person", "agent", "program"},
	FieldOwner:     {"person"},
	FieldAssignee:  {"person", "agent"},
	FieldWatcher:   {"person", "agent"},
	FieldMember:    {"person", "agent"},
}

// legacyIdentities maps the free-text spellings that exist in the ledger today
// to the identity they meant. Used by the backfill only; new writes are held to
// the strict form and "Michael" is refused.
var legacyIdentities = map[string]string{
	"michael":         "person:digital-michael",
	"digital-michael": "person:digital-michael",
}

// maxIdentityID bounds the id so a pasted paragraph is not accepted as a name.
const maxIdentityID = 128

// ParseIdentity checks s against the format and returns it in canonical form
// ("kind:id", kind and id lower case, no surrounding space). field selects
// which kinds are acceptable. An empty string is an error here; callers decide
// whether a field is optional before calling.
func ParseIdentity(field IdentityField, s string) (string, error) {
	allowed, ok := fieldKinds[field]
	if !ok {
		return "", fmt.Errorf("unknown people field %q", field)
	}
	v := strings.ToLower(strings.TrimSpace(s))
	kind, id, found := strings.Cut(v, ":")
	if !found || kind == "" || id == "" {
		return "", identityError(field, s, allowed)
	}
	if !contains(identityKinds, kind) || !contains(allowed, kind) {
		return "", identityError(field, s, allowed)
	}
	if len(id) > maxIdentityID || !validIdentityID(id) {
		return "", identityError(field, s, allowed)
	}
	return kind + ":" + id, nil
}

// LegacyIdentity resolves one of the old free-text spellings to its identity,
// for the backfill. ok is false for anything it does not recognise, so the
// caller can report the value instead of guessing.
func LegacyIdentity(s string) (string, bool) {
	id, ok := legacyIdentities[strings.ToLower(strings.TrimSpace(s))]
	return id, ok
}

// validIdentityID: lower-case letters, digits, and . _ - @ inside. Starts and
// ends with a letter or digit, so ids stay readable in URLs, filters and logs.
// "@" lets a remote ledger use an email-shaped login; ":" is excluded because
// it is the separator.
func validIdentityID(id string) bool {
	for i, r := range id {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		inner := r == '.' || r == '_' || r == '-' || r == '@'
		if !alnum && !(inner && i != 0 && i != len(id)-1) {
			return false
		}
	}
	return true
}

func identityError(field IdentityField, got string, allowed []string) error {
	return &FieldError{
		Field:   string(field) + " (as kind:id, e.g. person:digital-michael)",
		Value:   got,
		Allowed: prefixed(allowed),
	}
}

// prefixed turns kinds into the "kind:<id>" forms shown in the error text.
func prefixed(kinds []string) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = k + ":<id>"
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
