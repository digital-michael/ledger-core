// Usage notes for copy-ledger.
//
//	# rehearse: report what would be copied, change nothing
//	copy-ledger -from-db ~/.local/share/mcp-local/ledger.db -to-dsn 'postgres://u:p@host/db' -project website
//
//	# the selection is linked to other projects: the tool stops and names them. Then either
//	copy-ledger ... -project website -with-linked mcp-local,membership   # include these
//	copy-ledger ... -project website -drop-cross-links                   # leave every outside link out
//
//	# do it
//	copy-ledger ... -project website -drop-cross-links -apply
//
//	# confirm it, now or later: re-read both ledgers and compare every selected row
//	copy-ledger ... -project website -drop-cross-links -verify          # verify only, writes nothing
//	copy-ledger ... -project website -drop-cross-links -apply -verify   # copy, then verify from fresh reads
//
// Selecting: -all (every live project), -project a,b, -plan <id>, -workflow <id>; add
// -include-deleted for soft-deleted projects. Identities can be rewritten with
// -map-identity person:local=person:remote (repeatable).
//
// Exit status: 0 done (or a clean dry run, or a verification that matched), 1 error, 2 a
// decision about outside links is needed, 3 rows on the target differ from the source (a copy
// refused), 4 a verification found rows missing or different.

package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ledgercore "github.com/digital-michael/ledger-core"
)

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func main() {
	var projects, plans, workflows, withLinked, maps listFlag
	fromDB := flag.String("from-db", "", "source SQLite ledger file (default: LEDGER_DB_PATH, else ~/.local/share/mcp-local/ledger.db)")
	fromDSN := flag.String("from-dsn", "", "source PostgreSQL connection string, instead of -from-db")
	toDB := flag.String("to-db", "", "target SQLite ledger file")
	toDSN := flag.String("to-dsn", "", "target PostgreSQL connection string")
	all := flag.Bool("all", false, "copy every live project")
	includeDeleted := flag.Bool("include-deleted", false, "also copy soft-deleted projects")
	drop := flag.Bool("drop-cross-links", false, "leave out every link to a project that is not selected, and report them")
	skipInvalid := flag.Bool("skip-invalid", false, "leave out leaf rows (relations, notes, workflow links, members) whose value is outside the ledger vocabulary, and list them; tickets are never skipped")
	apply := flag.Bool("apply", false, "make the copy (default: report only)")
	verify := flag.Bool("verify", false, "confirm the target against the source: alone it only verifies and writes nothing; with -apply it copies and then verifies from fresh reads. Use the same selection options as the copy")
	flag.Var(&projects, "project", "project key, old key or id (comma-separated, repeatable)")
	flag.Var(&plans, "plan", "plan ticket id: copies its project and the projects of its members")
	flag.Var(&workflows, "workflow", "workflow id: copies its project and the projects it points at")
	flag.Var(&withLinked, "with-linked", "also copy these linked projects (the answer to the outside-links notice)")
	flag.Var(&maps, "map-identity", "from=to, e.g. person:local=person:remote (repeatable)")
	flag.Parse()

	mapping := map[string]string{}
	for _, m := range maps {
		from, to, ok := strings.Cut(m, "=")
		if !ok || from == "" || to == "" {
			fail(1, "-map-identity wants from=to, got %q", m)
		}
		mapping[from] = to
	}
	if (*toDB == "") == (*toDSN == "") {
		fail(1, "give exactly one target: -to-db or -to-dsn")
	}
	if *fromDB != "" && *fromDSN != "" {
		fail(1, "give one source: -from-db or -from-dsn")
	}

	ctx := context.Background()
	src, cleanup, err := openSource(ctx, *fromDB, *fromDSN)
	if err != nil {
		fail(1, "source: %v", err)
	}
	defer cleanup()
	if sameLedger(*fromDB, *fromDSN, *toDB, *toDSN) {
		fail(1, "the source and the target are the same ledger")
	}
	dst, err := ledgercore.Open(ledgercore.Options{Path: *toDB, DSN: *toDSN})
	if err != nil {
		fail(1, "target: %v", err)
	}
	defer dst.Close()

	opts := ledgercore.CopyOptions{
		Scope:          ledgercore.CopyScope{All: *all, Projects: projects, Plans: plans, Workflows: workflows, IncludeDeleted: *includeDeleted},
		WithLinked:     withLinked,
		DropCrossLinks: *drop,
		SkipInvalid:    *skipInvalid,
		MapIdentity:    mapping,
		Apply:          *apply,
	}

	// Verify only: read both ledgers, compare, write nothing.
	if *verify && !*apply {
		vrep, err := ledgercore.VerifyCopy(ctx, src, dst, opts)
		handle(err)
		printVerify(vrep)
		if !vrep.OK {
			os.Exit(4)
		}
		return
	}

	rep, err := ledgercore.CopyLedger(ctx, src, dst, opts)
	handle(err)
	print(rep)
	if *verify { // applied: confirm what is really on the target now, from fresh reads
		vrep, err := ledgercore.VerifyCopy(ctx, src, dst, opts)
		handle(err)
		fmt.Println()
		printVerify(vrep)
		if !vrep.OK {
			os.Exit(4)
		}
	}
}

// handle reports the errors that need a person, and exits; a nil error returns.
func handle(err error) {
	var cl *ledgercore.CrossLinksError
	var ce *ledgercore.CopyConflictError
	var ve *ledgercore.VocabularyError
	switch {
	case errors.As(err, &ve):
		fmt.Printf("%d row(s) hold a value outside the ledger vocabulary, which the target refuses:\n", len(ve.Rows))
		for _, r := range ve.Rows {
			fmt.Println("  " + r)
		}
		if ve.Skippable {
			fmt.Println("Nothing was copied. Fix them in the source, or leave them out with -skip-invalid.")
		} else {
			fmt.Println("Nothing was copied. Some are tickets or workflows, which cannot be left out: fix them in the source.")
		}
		os.Exit(4)
	case errors.As(err, &cl):
		fmt.Println("The selection is linked to projects that were not chosen:")
		for _, l := range cl.Links {
			fmt.Printf("  %-22s %-28s %-3s %d\n", l.Project, l.Kind, l.Direction, l.Count)
		}
		fmt.Println("Nothing was copied. Include some of them with -with-linked name[,name], or leave every")
		fmt.Println("outside link out with -drop-cross-links. Changing neither cancels.")
		os.Exit(2)
	case errors.As(err, &ce):
		fmt.Printf("%d row(s) on the target differ from the source; a copy never overwrites, so nothing was copied:\n", len(ce.Conflicts))
		for i, c := range ce.Conflicts {
			if i == 20 {
				fmt.Printf("  ... and %d more\n", len(ce.Conflicts)-20)
				break
			}
			fmt.Println("  " + c)
		}
		os.Exit(3)
	case err != nil:
		fail(1, "%v", err)
	}
}

func printVerify(v *ledgercore.VerifyReport) {
	fmt.Printf("verifying projects: %s\n\n", strings.Join(v.Projects, ", "))
	fmt.Printf("%-24s %9s %9s %8s %10s %14s\n", "table", "selected", "matching", "missing", "different", "only on target")
	for _, t := range v.Tables {
		if t.Selected == 0 && t.OnlyOnTarget == 0 {
			continue
		}
		fmt.Printf("%-24s %9d %9d %8d %10d %14d\n", t.Table, t.Selected, t.Matching, t.Missing, t.Different, t.OnlyOnTarget)
	}
	for _, t := range v.Tables {
		for _, e := range t.Examples {
			fmt.Println("  problem: " + e)
		}
	}
	if len(v.Dropped) > 0 {
		fmt.Println("\nlinks the selection leaves out (not expected on the target):")
		for _, l := range v.Dropped {
			fmt.Printf("  %-22s %-28s %-3s %d\n", l.Project, l.Kind, l.Direction, l.Count)
		}
	}
	if len(v.Skipped) > 0 {
		fmt.Printf("\n%d row(s) left out under -skip-invalid (outside the ledger vocabulary):\n", len(v.Skipped))
		for _, s := range v.Skipped {
			fmt.Println("  " + s)
		}
	}
	extra := 0
	for _, t := range v.Tables {
		extra += t.OnlyOnTarget
	}
	if extra > 0 {
		fmt.Printf("\n%d row(s) exist only on the target inside these projects (work done there since): information, not a failure.\n", extra)
	}
	if v.OK {
		fmt.Println("\nVERIFIED: every selected row is on the target and identical to the source.")
	} else {
		fmt.Println("\nMISMATCH: the target does not hold the selection as the source does (see the problems above).")
	}
}

func print(rep *ledgercore.CopyReport) {
	fmt.Printf("projects: %s\n\n", strings.Join(rep.Projects, ", "))
	fmt.Printf("%-24s %9s %9s %9s %13s\n", "table", "selected", "inserted", "updated", "already there")
	for _, t := range rep.Tables {
		if t.Selected == 0 {
			continue
		}
		fmt.Printf("%-24s %9d %9d %9d %13d\n", t.Table, t.Selected, t.Inserted, t.Updated, t.AlreadyThere)
	}
	if len(rep.Dropped) > 0 {
		fmt.Println("\nlinks left out (their other end was not selected):")
		for _, l := range rep.Dropped {
			fmt.Printf("  %-22s %-28s %-3s %d\n", l.Project, l.Kind, l.Direction, l.Count)
		}
	}
	if len(rep.Skipped) > 0 {
		fmt.Println("\nrows left out (outside the ledger vocabulary, -skip-invalid):")
		for _, s := range rep.Skipped {
			fmt.Println("  " + s)
		}
	}
	if rep.IdentitiesMapped > 0 {
		fmt.Printf("\nidentities rewritten: %d\n", rep.IdentitiesMapped)
	}
	if rep.Applied {
		fmt.Println("\ncopied and verified: every row was read back from the target and matches.")
	} else {
		fmt.Println("\ndry run: the copy ran in full and was rolled back. Nothing was changed (run with -apply).")
	}
}

// openSource opens the source ledger. A SQLite file is read from a snapshot taken with
// VACUUM INTO, so the source is never written to and never migrated.
func openSource(ctx context.Context, path, dsn string) (ledgercore.Store, func(), error) {
	if dsn != "" {
		s, err := ledgercore.Open(ledgercore.Options{DSN: dsn})
		return s, func() { s.Close() }, err
	}
	if path == "" {
		p, err := ledgercore.DefaultPath()
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
	s, err := ledgercore.Open(ledgercore.Options{Path: snap})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return s, func() { s.Close(); cleanup() }, nil
}

func sameLedger(fromDB, fromDSN, toDB, toDSN string) bool {
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

func fail(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "copy-ledger: "+format+"\n", args...)
	os.Exit(code)
}
