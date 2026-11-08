package ledgercore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Exit codes a front end gives the outcomes of a copy that need a person.
const (
	CopyExitDecision = 2 // outside links or off-vocabulary rows need a decision
	CopyExitConflict = 3 // rows on the target differ from the source
	CopyExitVerify   = 4 // a verification found rows missing or different
)

// OpenCopySource opens the ledger a copy reads from. A SQLite file is read from a snapshot taken
// with VACUUM INTO, so the source is never written to and never migrated; an empty path means
// the default ledger. The returned func removes the snapshot.
func OpenCopySource(ctx context.Context, path, dsn string) (Store, func(), error) {
	if dsn != "" {
		s, err := Open(Options{DSN: dsn})
		if err != nil {
			return nil, nil, err
		}
		return s, func() { s.Close() }, nil
	}
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, nil, err
		}
		path = p
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "copy-ledger-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	snap := filepath.Join(dir, "snapshot.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	_, err = raw.ExecContext(ctx, "VACUUM INTO ?", snap)
	raw.Close()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("snapshotting %s: %w", path, err)
	}
	s, err := Open(Options{Path: snap})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return s, func() { s.Close(); cleanup() }, nil
}

// SameLedger reports whether a source and a target name the same ledger, which a copy refuses.
func SameLedger(fromDB, fromDSN, toDB, toDSN string) bool {
	if fromDSN != "" && fromDSN == toDSN {
		return true
	}
	if fromDB != "" && toDB != "" {
		a, _ := filepath.Abs(fromDB)
		b, _ := filepath.Abs(toDB)
		return a == b
	}
	return false
}

func linkLines(b *strings.Builder, links []CrossLink) {
	for _, l := range links {
		fmt.Fprintf(b, "  %-22s %-28s %-3s %d\n", l.Project, l.Kind, l.Direction, l.Count)
	}
}

// Text is the report in words, for a terminal or a tool result.
func (r *CopyReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "projects: %s\n\n", strings.Join(r.Projects, ", "))
	fmt.Fprintf(&b, "%-24s %9s %9s %9s %13s\n", "table", "selected", "inserted", "updated", "already there")
	for _, t := range r.Tables {
		if t.Selected == 0 {
			continue
		}
		fmt.Fprintf(&b, "%-24s %9d %9d %9d %13d\n", t.Table, t.Selected, t.Inserted, t.Updated, t.AlreadyThere)
	}
	if len(r.Dropped) > 0 {
		b.WriteString("\nlinks left out (their other end was not selected):\n")
		linkLines(&b, r.Dropped)
	}
	if len(r.Skipped) > 0 {
		b.WriteString("\nrows left out (outside the ledger vocabulary, -skip-invalid):\n")
		for _, s := range r.Skipped {
			b.WriteString("  " + s + "\n")
		}
	}
	if r.IdentitiesMapped > 0 {
		fmt.Fprintf(&b, "\nidentities rewritten: %d\n", r.IdentitiesMapped)
	}
	if r.Applied {
		b.WriteString("\ncopied and verified: every row was read back from the target and matches.\n")
	} else {
		b.WriteString("\ndry run: the copy ran in full and was rolled back. Nothing was changed (run with -apply).\n")
	}
	return b.String()
}

// Text is the verification in words, for a terminal or a tool result.
func (v *VerifyReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "verifying projects: %s\n\n", strings.Join(v.Projects, ", "))
	fmt.Fprintf(&b, "%-24s %9s %9s %8s %10s %14s\n", "table", "selected", "matching", "missing", "different", "only on target")
	for _, t := range v.Tables {
		if t.Selected == 0 && t.OnlyOnTarget == 0 {
			continue
		}
		fmt.Fprintf(&b, "%-24s %9d %9d %8d %10d %14d\n", t.Table, t.Selected, t.Matching, t.Missing, t.Different, t.OnlyOnTarget)
	}
	for _, t := range v.Tables {
		for _, e := range t.Examples {
			b.WriteString("  problem: " + e + "\n")
		}
	}
	if len(v.Dropped) > 0 {
		b.WriteString("\nlinks the selection leaves out (not expected on the target):\n")
		linkLines(&b, v.Dropped)
	}
	if len(v.Skipped) > 0 {
		fmt.Fprintf(&b, "\n%d row(s) left out under -skip-invalid (outside the ledger vocabulary):\n", len(v.Skipped))
		for _, s := range v.Skipped {
			b.WriteString("  " + s + "\n")
		}
	}
	extra := 0
	for _, t := range v.Tables {
		extra += t.OnlyOnTarget
	}
	if extra > 0 {
		fmt.Fprintf(&b, "\n%d row(s) exist only on the target inside these projects (work done there since): information, not a failure.\n", extra)
	}
	if v.OK {
		b.WriteString("\nVERIFIED: every selected row is on the target and identical to the source.\n")
	} else {
		b.WriteString("\nMISMATCH: the target does not hold the selection as the source does (see the problems above).\n")
	}
	return b.String()
}

// CopyDecisionText describes an outcome that needs a person (outside links, rows outside the
// vocabulary, rows that differ) and the exit code a front end gives it. ok is false for any
// other error, which the caller reports as a failure.
func CopyDecisionText(err error) (text string, code int, ok bool) {
	var cl *CrossLinksError
	var ce *CopyConflictError
	var ve *VocabularyError
	var b strings.Builder
	switch {
	case errors.As(err, &cl):
		b.WriteString("The selection is linked to projects that were not chosen:\n")
		linkLines(&b, cl.Links)
		b.WriteString("Nothing was copied. Include some of them with -with-linked name[,name], or leave every\n")
		b.WriteString("outside link out with -drop-cross-links. Changing neither cancels.\n")
		return b.String(), CopyExitDecision, true
	case errors.As(err, &ve):
		fmt.Fprintf(&b, "%d row(s) hold a value outside the ledger vocabulary, which the target refuses:\n", len(ve.Rows))
		for _, r := range ve.Rows {
			b.WriteString("  " + r + "\n")
		}
		if ve.Skippable {
			b.WriteString("Nothing was copied. Fix them in the source, or leave them out with -skip-invalid.\n")
		} else {
			b.WriteString("Nothing was copied. Some are tickets or workflows, which cannot be left out: fix them in the source.\n")
		}
		return b.String(), CopyExitDecision, true
	case errors.As(err, &ce):
		fmt.Fprintf(&b, "%d row(s) on the target differ from the source; a copy never overwrites, so nothing was copied:\n", len(ce.Conflicts))
		for i, c := range ce.Conflicts {
			if i == 20 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(ce.Conflicts)-20)
				break
			}
			b.WriteString("  " + c + "\n")
		}
		return b.String(), CopyExitConflict, true
	}
	return "", 0, false
}
