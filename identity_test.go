package ledgercore

import (
	"errors"
	"strings"
	"testing"
)

func TestParseIdentity_acceptsCanonicalAndNormalises(t *testing.T) {
	cases := []struct {
		field IdentityField
		in    string
		want  string
	}{
		{FieldOwner, "person:digital-michael", "person:digital-michael"},
		{FieldOwner, "  Person:Digital-Michael  ", "person:digital-michael"},
		{FieldAssignee, "agent:claude-code", "agent:claude-code"},
		{FieldAssignee, "agent:other-agent.v2", "agent:other-agent.v2"}, // agents are many; none special
		{FieldCreatedBy, "program:mcp-console", "program:mcp-console"},
		{FieldMember, "person:someone@example.org", "person:someone@example.org"},
		{FieldWatcher, "person:a", "person:a"},
	}
	for _, c := range cases {
		got, err := ParseIdentity(c.field, c.in)
		if err != nil {
			t.Errorf("%s %q: unexpected error %v", c.field, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s %q = %q, want %q", c.field, c.in, got, c.want)
		}
	}
}

func TestParseIdentity_refusesBadShape(t *testing.T) {
	bad := []string{
		"", "   ", "digital-michael", "Michael", "person:", ":digital-michael",
		"robot:x", "person:has space", "person:two:colons", "person:-lead", "person:trail-",
		"person:.dot", "person:ünï", "person:a/b", "person:" + strings.Repeat("a", maxIdentityID+1),
	}
	for _, in := range bad {
		_, err := ParseIdentity(FieldAssignee, in)
		if err == nil {
			t.Errorf("%q was accepted", in)
			continue
		}
		var fe *FieldError
		if !errors.As(err, &fe) || !errors.Is(err, ErrInvalidField) {
			t.Errorf("%q: error %v is not a *FieldError wrapping ErrInvalidField", in, err)
		}
	}
}

func TestParseIdentity_kindRulesPerField(t *testing.T) {
	// owner is accountable: a person only.
	for _, in := range []string{"agent:claude-code", "program:mcp"} {
		if _, err := ParseIdentity(FieldOwner, in); err == nil {
			t.Errorf("owner accepted %q", in)
		}
	}
	// a bare program has no stake as assignee, watcher or member...
	for _, f := range []IdentityField{FieldAssignee, FieldWatcher, FieldMember} {
		if _, err := ParseIdentity(f, "program:mcp"); err == nil {
			t.Errorf("%s accepted a program", f)
		}
	}
	// ...but created_by records who actually wrote it, so it may be one.
	if _, err := ParseIdentity(FieldCreatedBy, "program:mcp"); err != nil {
		t.Errorf("created_by refused a program: %v", err)
	}
	// agents may be assigned and may watch.
	for _, f := range []IdentityField{FieldAssignee, FieldWatcher} {
		if _, err := ParseIdentity(f, "agent:claude-code"); err != nil {
			t.Errorf("%s refused an agent: %v", f, err)
		}
	}
}

func TestParseIdentity_unknownFieldIsAnError(t *testing.T) {
	if _, err := ParseIdentity("nope", "person:x"); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestParseIdentity_errorNamesTheExpectedForm(t *testing.T) {
	_, err := ParseIdentity(FieldOwner, "Michael")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"owner", "person:digital-michael", "person:<id>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

func TestLegacyIdentity(t *testing.T) {
	for _, in := range []string{"Michael", "digital-michael", " DIGITAL-MICHAEL "} {
		got, ok := LegacyIdentity(in)
		if !ok || got != "person:digital-michael" {
			t.Errorf("LegacyIdentity(%q) = %q, %v", in, got, ok)
		}
	}
	// Anything unknown is reported, never guessed.
	if got, ok := LegacyIdentity("someone else"); ok {
		t.Errorf("guessed %q for an unknown value", got)
	}
	// The strict parser still refuses the legacy spelling on new writes.
	if _, err := ParseIdentity(FieldAssignee, "Michael"); err == nil {
		t.Error("new writes must not accept the legacy spelling")
	}
}

// Every canonical kind is one the Actor type already knows, so a people field
// and audit_log.actor can never disagree about what a kind is.
func TestIdentityKinds_matchActorKinds(t *testing.T) {
	for _, k := range []ActorKind{ActorPerson, ActorAgent, ActorProgram} {
		if !contains(identityKinds, string(k)) {
			t.Errorf("identity kinds are missing %q", k)
		}
	}
	if len(identityKinds) != 3 {
		t.Errorf("identity kinds = %v; a new kind is a schema decision (U4)", identityKinds)
	}
	// Round trip: an Actor's stored form parses back to itself.
	a := Actor{Kind: ActorAgent, ID: "claude-code"}
	if got, err := ParseIdentity(FieldCreatedBy, a.String()); err != nil || got != a.String() {
		t.Errorf("Actor.String() %q did not round-trip: %q, %v", a.String(), got, err)
	}
}
