# ledger-core

The ledger's persistence layer: one implementation of every ledger read and write, shared by every
program that touches the ledger database.

| Consumer | How it uses ledger-core |
|---|---|
| `mcp-local` | Its `internal/store/ledger` package is a thin adapter: type aliases, plus an `Open()` that supplies the calling MCP client's name for the audit log |
| `ledger-server` (planned) | Web UI for the ledger — imports this package directly |

Extracted from `mcp-local/internal/store/ledger` on 2026-09-10, with its git history, so the MCP tools
and the web UI cannot drift apart on validation, audit logging, soft delete or id resolution.
Design record: `cortex/docs/ledger.md`. Tracking: ledger project `ledger-server`, epic `d28ed3b2`.

## Using it

```go
import ledgercore "github.com/digital-michael/ledger-core"

store, err := ledgercore.Open(ledgercore.Options{
    // Path: "" means DefaultPath(): LEDGER_DB_PATH, else ~/.local/share/mcp-local/ledger.db
    Client: func(ctx context.Context) string { return "my-program" }, // recorded on every audit row
})
```

`ledgercore.Vocab()` returns the valid item types, statuses, relation types and note types, so a
caller can offer only valid choices.

## Invariants — do not change these casually

- **The default path stays `~/.local/share/mcp-local/ledger.db`.** The `mcp-local` directory name
  is historical. Every program must open the same file; renaming it would point new builds at an
  empty database.
- **Pragmas are set in the DSN, never with `Exec()` after opening.** `busy_timeout` must apply to
  every pooled connection and before the first `Ping()`. Without it, 923 of 1000 concurrent writes
  failed (measured 2026-09-07).
- **Vocabulary is enforced twice from one source** (`vocabulary.go`): by Go validation, and by
  SQLite triggers generated from the same lists (`enforce.go`). Triggers, not `CHECK` constraints,
  because adding a `CHECK` to an existing SQLite table means rebuilding it.
- **Every mutation writes its audit row in the same transaction**, with non-empty detail for
  creates and updates. `audit_log` is append-only: no update or delete path exists.
- **Timer events are history.** Only `StartTimer`/`StopTimer` write them; `AddNote` writes comments
  only; `UpdateNote` refuses timer events.

## Testing

```
go test ./...          # full suite, including the multi-process concurrency regression test
go test -short ./...   # skip the test that spawns child processes
```

The tests open real SQLite databases in temp directories through the public `Open()`. The
concurrency test re-executes the test binary as 5 separate writer processes, because SQLite's
cross-process locking is what's under test.

## Status

Not yet published to GitHub. `mcp-local` consumes it through a `replace => ../ledger-core`
directive, so **`mcp-local` must not be pushed until this repo is published** and that directive
is swapped for a pinned version.
