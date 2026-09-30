package ledgercore

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A workflow's content: how a goal is achieved, as a use case -- actor,
// goal, parameters, preconditions, ordered steps with expected results,
// alternate (failure) paths and postconditions. It is the unit that is
// versioned: each saved WorkflowDoc is one immutable version.
//
// Every addressable part carries a Key that stays the same across versions,
// so a ticket linked to a step is still linked to that step after steps are
// inserted, removed or reordered. Keys are assigned by this package, never
// reused within a workflow, and are human-readable:
//
//	i1, i2        in scope        o1, o2        not in scope
//	f1, f2        features        c1, c2        constraints
//	p1, p2        preconditions
//	s1, s2        steps           s2.r1, s2.r2  a step's expected results
//	a1, a2        alternate paths a1.r1         an alternate path's results
//	q1, q2        postconditions
//
// A caller editing a workflow sends the keys it got back and omits them on
// anything new. A key this workflow has never issued is refused: inventing
// one would silently attach a new step to an old step's tickets.
type WorkflowDoc struct {
	Title          string              `json:"title"`
	Goal           string              `json:"goal"`
	Actor          string              `json:"actor,omitempty"`
	InScope        []WorkflowItem      `json:"in_scope,omitempty"`
	OutOfScope     []WorkflowItem      `json:"out_of_scope,omitempty"`
	Features       []WorkflowItem      `json:"features,omitempty"`
	Constraints    []WorkflowItem      `json:"constraints,omitempty"`
	Parameters     []WorkflowParam     `json:"parameters,omitempty"`
	Preconditions  []WorkflowCondition `json:"preconditions,omitempty"`
	Steps          []WorkflowStep      `json:"steps"`
	AlternatePaths []WorkflowAltPath   `json:"alternate_paths,omitempty"`
	Postconditions []WorkflowCondition `json:"postconditions,omitempty"`
}

// WorkflowItem is one entry in a workflow-level list: what is in or out of
// scope, a feature the workflow delivers, a constraint it must respect.
// Features need building, so an unlinked feature is a gap; scope and
// constraints are checked rather than built, so they never are -- all four
// can still carry ticket links.
type WorkflowItem struct {
	Key  string `json:"key,omitempty"`
	Text string `json:"text"`
}

// WorkflowParam is a named value the steps refer to as {name}, so one
// workflow runs against local and remote alike.
type WorkflowParam struct {
	Name        string `json:"name"`
	Value       string `json:"value,omitempty"`
	Description string `json:"description,omitempty"`
}

// WorkflowRef points at another workflow, following its current version
// unless Version pins one.
type WorkflowRef struct {
	WorkflowID string `json:"workflow_id"`
	Version    int    `json:"version,omitempty"`
}

// WorkflowCondition is a precondition or postcondition: text, or a reference
// to another workflow ("Admin signs in" defined once, reused everywhere).
type WorkflowCondition struct {
	Key  string       `json:"key,omitempty"`
	Text string       `json:"text,omitempty"`
	Ref  *WorkflowRef `json:"ref,omitempty"`
}

// WorkflowStep is one action and what should follow from it. A step with a
// Ref means "perform that workflow" -- the way a long workflow is decomposed
// into ones short enough to read.
type WorkflowStep struct {
	Key     string           `json:"key,omitempty"`
	Action  string           `json:"action,omitempty"`
	Ref     *WorkflowRef     `json:"ref,omitempty"`
	Results []WorkflowResult `json:"results,omitempty"`
}

// WorkflowResult is one expected result.
type WorkflowResult struct {
	Key  string `json:"key,omitempty"`
	Text string `json:"text"`
}

// WorkflowAltPath is what happens when something goes wrong at a step.
type WorkflowAltPath struct {
	Key       string           `json:"key,omitempty"`
	AtStep    string           `json:"at_step"`
	Condition string           `json:"condition"`
	Results   []WorkflowResult `json:"results,omitempty"`
}

var paramNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// keyedParts lists every key a document uses, with a short description of
// what it names. The order is document order.
func (d *WorkflowDoc) keyedParts() []keyedPart {
	var parts []keyedPart
	for _, sec := range d.itemSections() {
		for _, it := range *sec.items {
			parts = append(parts, keyedPart{it.Key, sec.kind})
		}
	}
	for _, c := range d.Preconditions {
		parts = append(parts, keyedPart{c.Key, "precondition"})
	}
	for _, s := range d.Steps {
		parts = append(parts, keyedPart{s.Key, "step"})
		for _, r := range s.Results {
			parts = append(parts, keyedPart{r.Key, "result"})
		}
	}
	for _, a := range d.AlternatePaths {
		parts = append(parts, keyedPart{a.Key, "alternate"})
		for _, r := range a.Results {
			parts = append(parts, keyedPart{r.Key, "alternate_result"})
		}
	}
	for _, c := range d.Postconditions {
		parts = append(parts, keyedPart{c.Key, "postcondition"})
	}
	return parts
}

type keyedPart struct{ key, kind string }

// itemSection is one workflow-level list of WorkflowItems.
type itemSection struct {
	items  *[]WorkflowItem
	prefix string // key prefix: i, o, f, c
	kind   string // in_scope, out_of_scope, feature, constraint
	label  string // for messages
}

// itemSections lists the workflow-level lists in document order.
func (d *WorkflowDoc) itemSections() []itemSection {
	return []itemSection{
		{&d.InScope, "i", "in_scope", "in-scope item"},
		{&d.OutOfScope, "o", "out_of_scope", "not-in-scope item"},
		{&d.Features, "f", "feature", "feature"},
		{&d.Constraints, "c", "constraint", "constraint"},
	}
}

// refs returns every workflow reference in the document.
func (d *WorkflowDoc) refs() []WorkflowRef {
	var out []WorkflowRef
	for _, c := range d.Preconditions {
		if c.Ref != nil {
			out = append(out, *c.Ref)
		}
	}
	for _, s := range d.Steps {
		if s.Ref != nil {
			out = append(out, *s.Ref)
		}
	}
	for _, c := range d.Postconditions {
		if c.Ref != nil {
			out = append(out, *c.Ref)
		}
	}
	return out
}

// validateShape checks everything that needs no database: required text,
// parameter names, alternate paths pointing at a real step. References and
// keys are checked against the database separately.
func (d *WorkflowDoc) validateShape() error {
	if strings.TrimSpace(d.Title) == "" {
		return docError("title is required")
	}
	if strings.TrimSpace(d.Goal) == "" {
		return docError("goal is required")
	}
	if len(d.Steps) == 0 {
		return docError("a workflow needs at least one step")
	}
	seen := map[string]bool{}
	for i, p := range d.Parameters {
		if !paramNameRE.MatchString(p.Name) {
			return docError("parameter %d: name %q must be lowercase letters, digits and underscores, starting with a letter or underscore", i+1, p.Name)
		}
		if seen[p.Name] {
			return docError("parameter %q is defined twice", p.Name)
		}
		seen[p.Name] = true
	}
	for _, sec := range d.itemSections() {
		for i, it := range *sec.items {
			if strings.TrimSpace(it.Text) == "" {
				return docError("%s %d: text is required", sec.label, i+1)
			}
		}
	}
	for i, c := range d.Preconditions {
		if err := checkCondition("precondition", i, c); err != nil {
			return err
		}
	}
	for i, c := range d.Postconditions {
		if err := checkCondition("postcondition", i, c); err != nil {
			return err
		}
	}
	for i, s := range d.Steps {
		hasAction := strings.TrimSpace(s.Action) != ""
		if !hasAction && s.Ref == nil {
			return docError("step %d needs an action or a workflow reference", i+1)
		}
		for j, r := range s.Results {
			if strings.TrimSpace(r.Text) == "" {
				return docError("step %d, result %d: text is required", i+1, j+1)
			}
		}
	}
	for i, a := range d.AlternatePaths {
		if strings.TrimSpace(a.Condition) == "" {
			return docError("alternate path %d: condition is required", i+1)
		}
		if a.AtStep == "" {
			return docError("alternate path %d: at_step is required (a step key such as s3, or a step number)", i+1)
		}
		for j, r := range a.Results {
			if strings.TrimSpace(r.Text) == "" {
				return docError("alternate path %d, result %d: text is required", i+1, j+1)
			}
		}
	}
	return nil
}

func checkCondition(kind string, i int, c WorkflowCondition) error {
	if strings.TrimSpace(c.Text) == "" && c.Ref == nil {
		return docError("%s %d needs text or a workflow reference", kind, i+1)
	}
	if c.Ref != nil && c.Ref.Version < 0 {
		return docError("%s %d: version must be positive, or omitted for the current one", kind, i+1)
	}
	return nil
}

// assignKeys gives every part without a key a new one, and refuses a key the
// workflow never issued or one used twice in the same document. issued holds
// every key any earlier version of this workflow used; it is what makes keys
// never-reused, even after the part they named was removed.
//
// Alternate paths are resolved last, because at_step names a step and a
// brand-new step only has a key once this function has run. at_step may
// therefore be a step key ("s3") or the step's 1-based position ("3"); a
// position is rewritten to the key, so the stored document always holds keys.
func (d *WorkflowDoc) assignKeys(issued map[string]bool) error {
	inDoc := map[string]bool{}
	claim := func(key, what string) error {
		if !issued[key] {
			return docError("%s key %q was never issued by this workflow; omit the key for a new %s", what, key, what)
		}
		if inDoc[key] {
			return docError("key %q is used twice", key)
		}
		inDoc[key] = true
		return nil
	}
	// Claim every existing key first, so a new key can never collide with an
	// existing one that appears later in the document.
	for _, sec := range d.itemSections() {
		for _, it := range *sec.items {
			if it.Key != "" {
				if err := claim(it.Key, sec.label); err != nil {
					return err
				}
			}
		}
	}
	for _, c := range d.Preconditions {
		if c.Key != "" {
			if err := claim(c.Key, "precondition"); err != nil {
				return err
			}
		}
	}
	for _, s := range d.Steps {
		if s.Key != "" {
			if err := claim(s.Key, "step"); err != nil {
				return err
			}
		}
		for _, r := range s.Results {
			if r.Key != "" {
				if err := claim(r.Key, "result"); err != nil {
					return err
				}
			}
		}
	}
	for _, a := range d.AlternatePaths {
		if a.Key != "" {
			if err := claim(a.Key, "alternate path"); err != nil {
				return err
			}
		}
		for _, r := range a.Results {
			if r.Key != "" {
				if err := claim(r.Key, "result"); err != nil {
					return err
				}
			}
		}
	}
	for _, c := range d.Postconditions {
		if c.Key != "" {
			if err := claim(c.Key, "postcondition"); err != nil {
				return err
			}
		}
	}

	used := func(key string) bool { return issued[key] || inDoc[key] }
	next := func(prefix string) string {
		for n := 1; ; n++ {
			k := prefix + strconv.Itoa(n)
			if !used(k) {
				inDoc[k] = true
				return k
			}
		}
	}
	for _, sec := range d.itemSections() {
		items := *sec.items
		for i := range items {
			if items[i].Key == "" {
				items[i].Key = next(sec.prefix)
			}
		}
	}
	for i := range d.Preconditions {
		if d.Preconditions[i].Key == "" {
			d.Preconditions[i].Key = next("p")
		}
	}
	for i := range d.Steps {
		s := &d.Steps[i]
		if s.Key == "" {
			s.Key = next("s")
		}
		for j := range s.Results {
			if s.Results[j].Key == "" {
				s.Results[j].Key = next(s.Key + ".r")
			} else if !strings.HasPrefix(s.Results[j].Key, s.Key+".") {
				return docError("result %q belongs to another step, not %s", s.Results[j].Key, s.Key)
			}
		}
	}
	steps := map[string]bool{}
	for _, s := range d.Steps {
		steps[s.Key] = true
	}
	for i := range d.AlternatePaths {
		a := &d.AlternatePaths[i]
		if n, err := strconv.Atoi(a.AtStep); err == nil {
			if n < 1 || n > len(d.Steps) {
				return docError("alternate path %d: at_step %d is not a step in this workflow (it has %d)", i+1, n, len(d.Steps))
			}
			a.AtStep = d.Steps[n-1].Key
		}
		if !steps[a.AtStep] {
			return docError("alternate path %d: at_step %q is not a step in this workflow", i+1, a.AtStep)
		}
		if a.Key == "" {
			a.Key = next("a")
		}
		for j := range a.Results {
			if a.Results[j].Key == "" {
				a.Results[j].Key = next(a.Key + ".r")
			} else if !strings.HasPrefix(a.Results[j].Key, a.Key+".") {
				return docError("result %q belongs to another alternate path, not %s", a.Results[j].Key, a.Key)
			}
		}
	}
	for i := range d.Postconditions {
		if d.Postconditions[i].Key == "" {
			d.Postconditions[i].Key = next("q")
		}
	}
	return nil
}

// docError is an invalid workflow document. It classifies as ErrInvalidField
// so a web caller answers 400, while the message reads as-is to an agent.
func docError(format string, args ...any) error {
	return &lookupError{msg: "workflow: " + fmt.Sprintf(format, args...), kind: ErrInvalidField}
}
