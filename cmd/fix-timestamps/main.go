// Usage notes for fix-timestamps.
//
// Without -apply it only reports. With -apply it rewrites the stamps, all or nothing.
// Take a backup first.
//
//	go run ./cmd/fix-timestamps -db /path/to/ledger.db            # report only
//	go run ./cmd/fix-timestamps -db /path/to/ledger.db -apply     # rewrite

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	ledgercore "github.com/digital-michael/ledger-core"
)

func main() {
	path := flag.String("db", "", "ledger database (default: LEDGER_DB_PATH, else ~/.local/share/mcp-local/ledger.db)")
	apply := flag.Bool("apply", false, "rewrite the stamps (default: report only)")
	flag.Parse()

	store, err := ledgercore.Open(ledgercore.Options{Path: *path})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer store.Close()

	rep, err := store.NormalizeTimestamps(context.Background(), *apply)
	if rep != nil {
		fmt.Printf("%-26s %-12s %8s %8s %10s\n", "table", "column", "values", "fixed", "to rewrite")
		for _, c := range rep.Columns {
			if c.NonNull == 0 {
				continue
			}
			fmt.Printf("%-26s %-12s %8d %8d %10d\n", c.Table, c.Column, c.NonNull, c.Fixed, c.Rewritten)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fix-timestamps:", err)
		os.Exit(1)
	}
	switch {
	case rep.Applied:
		fmt.Printf("rewrote %d stamp(s); every value is now fixed width.\n", rep.Rewritten)
	case rep.Rewritten == 0:
		fmt.Println("every stamp is already fixed width: nothing to do.")
	default:
		fmt.Printf("%d stamp(s) would be rewritten. Nothing changed (run with -apply).\n", rep.Rewritten)
	}
}
