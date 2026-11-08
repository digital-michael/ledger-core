// Usage notes for drop-legacy-label: removes the old items.label column once every label is
// safely in item_labels.
//
// Without -apply it only prints the zero-loss check and changes nothing. With
// -apply it drops the column, and only if the check passes. Take a backup
// first, and make sure no program still on the old release has the ledger open.
//
//	go run ./cmd/drop-legacy-label -db /path/to/ledger.db            # check only
//	go run ./cmd/drop-legacy-label -db /path/to/ledger.db -apply     # check, then drop

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
	apply := flag.Bool("apply", false, "drop the column if the check passes (default: check only)")
	flag.Parse()

	store, err := ledgercore.Open(ledgercore.Options{Path: *path})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer store.Close()
	ctx := context.Background()

	rep, err := store.LabelMigrationReport(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		os.Exit(1)
	}
	if len(rep.Projects) == 0 {
		fmt.Println("items.label is already gone (or no item ever had one): nothing to check.")
		return
	}
	fmt.Printf("zero-loss check: %d legacy labels, %d represented in item_labels\n", rep.Legacy, rep.Copied)
	for _, p := range rep.Projects {
		mark := "ok"
		if p.Copied != p.Legacy {
			mark = "MISMATCH"
		}
		fmt.Printf("  %-26s legacy=%-5d in item_labels=%-5d %s\n", p.Project, p.Legacy, p.Copied, mark)
	}
	if !rep.OK() {
		fmt.Fprintln(os.Stderr, "the check FAILED: not dropping anything")
		os.Exit(2)
	}
	if !*apply {
		fmt.Println("the check passed. Nothing changed (run with -apply to drop items.label).")
		return
	}
	dropped, err := store.DropLegacyLabelColumn(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "drop:", err)
		os.Exit(1)
	}
	if dropped {
		fmt.Println("items.label dropped.")
	} else {
		fmt.Println("items.label was already gone.")
	}
}
