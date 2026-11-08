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
// decision is needed (outside links, or rows outside the vocabulary), 3 rows on the target differ from the source (a copy
// refused), 4 a verification found rows missing or different.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
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
	src, cleanup, err := ledgercore.OpenCopySource(ctx, *fromDB, *fromDSN)
	if err != nil {
		fail(1, "source: %v", err)
	}
	defer cleanup()
	if ledgercore.SameLedger(*fromDB, *fromDSN, *toDB, *toDSN) {
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
			os.Exit(ledgercore.CopyExitVerify)
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
			os.Exit(ledgercore.CopyExitVerify)
		}
	}
}

// handle reports the errors that need a person, and exits; a nil error returns.
func handle(err error) {
	if err == nil {
		return
	}
	if text, code, ok := ledgercore.CopyDecisionText(err); ok {
		fmt.Print(text)
		os.Exit(code)
	}
	fail(1, "%v", err)
}

func printVerify(v *ledgercore.VerifyReport) { fmt.Print(v.Text()) }

func print(rep *ledgercore.CopyReport) { fmt.Print(rep.Text()) }

func fail(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "copy-ledger: "+format+"\n", args...)
	os.Exit(code)
}
