// backfill-people fills created_by and owner on the ledger's existing items and
// projects, and gives every project without one a first owner (ledger story
// a94140e8).
//
// created_by comes from the first "created" audit row of each item or project
// (see ledgercore.CreatorFromAudit for the mapping); owner is the person given
// with -owner. Only empty values are filled, so a value already set is never
// overwritten and running it twice changes nothing the second time.
//
// Without -apply it only reports what it would do. Take a backup first.
//
//	go run ./cmd/backfill-people -db /path/to/ledger.db -owner person:digital-michael           # report
//	go run ./cmd/backfill-people -db /path/to/ledger.db -owner person:digital-michael -apply    # do it
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	ledgercore "github.com/digital-michael/ledger-core"
)

func main() {
	path := flag.String("db", "", "ledger database (default: LEDGER_DB_PATH, else ~/.local/share/mcp-local/ledger.db)")
	owner := flag.String("owner", "person:digital-michael", "the person who owns everything that has no owner yet")
	apply := flag.Bool("apply", false, "make the changes (default: report only)")
	flag.Parse()

	store, err := ledgercore.Open(ledgercore.Options{Path: *path})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer store.Close()

	rep, err := store.BackfillPeople(context.Background(), ledgercore.BackfillOptions{Owner: *owner, Apply: *apply})
	if err != nil {
		fmt.Fprintln(os.Stderr, "backfill:", err)
		os.Exit(1)
	}
	verb := "would change"
	if rep.Applied {
		verb = "changed"
	}
	fmt.Printf("%s: %d items, %d projects given a creator, %d projects given a first owner (%s)\n",
		verb, rep.Items, rep.Projects, rep.OwnersAdded, *owner)
	printCounts("created_by came from", rep.BySource)
	printCounts("created_by values", rep.ByCreator)
	if !rep.Applied {
		fmt.Println("Nothing was written (run with -apply to make these changes).")
	}
}

func printCounts(title string, m map[string]int) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println(title + ":")
	for _, k := range keys {
		fmt.Printf("  %-28s %d\n", k, m[k])
	}
}
