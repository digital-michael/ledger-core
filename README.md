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

## Status and releases

Published at `github.com/digital-michael/ledger-core` with semver tags (annotated, with a
one-paragraph summary). Consumers -- `mcp-local` and `ledger-server` -- pin a tagged version in
`go.mod`, so a build outside this workspace (another machine, CI, a container image for a remote
deployment) is reproducible.

Local development across the three repos uses `cortex/go.work`, which is not committed: it
builds the consumers against this checkout. A change here is therefore live locally before it is
released, and **not** in any standalone build until it is. After a change lands:

1. Tag a release: patch for fixes and behaviour corrections with no API change, minor for new
   API. Push the tag.
2. Bump the pin in each consumer (`GOWORK=off go get github.com/digital-michael/ledger-core@vX.Y.Z`,
   then `GOWORK=off go mod tidy`), and confirm `GOWORK=off go build ./...` and the tests pass.
