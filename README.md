# ledger-core

The ledger's persistence layer: one SQLite-backed implementation of every ledger read and write,
shared by every program that touches the ledger database.

## Purpose

A ledger is a work-tracking store: projects, work items, notes, timers, relations, versioned
workflows and an audit trail. `ledger-core` is the only code that reads or writes it. The programs
in front of it (an MCP server, a web UI) supply a transport and say who is writing; the rules live
here.

## Goals

- **One implementation of every write.** Vocabulary validation, the audit row in the same
  transaction, soft delete, id-prefix resolution and conflict detection on edits happen once, so
  the front doors cannot drift apart. This serves the platform goal of giving models real tools
  (goal 3), since the ledger is one of those tool domains.
- **One identity format for every people field.** A creator, an owner, an assignee, a watcher and a
  project member are all written `kind:id` (`person:alice`, `agent:claude-code`), and every list is
  held the same way.
- **Safe to share between processes on one machine.** Write-ahead logging, a busy timeout on every
  connection, and a regression test that writes from separate processes.
- **One seam for access rules.** `Options.Permit` is where per-project or per-role rules attach,
  rather than being retrofitted across every write path.
- TODO(operator): which platform goal does a shared, multi-user ledger on a remote host serve?
  `goals.md` names none of its seven goals for it.

## Non-goals

- **No transport.** It knows nothing about MCP or HTTP. The caller says who is writing, through
  `Options.Actor`.
- **No authentication and no user registry.** It checks that an identity is well formed, not that
  the person exists.
- **No notification delivery.** Watchers are stored; nothing is sent.
- **No external synchronisation, and no database other than SQLite today.**

## Boundaries

| | |
|---|---|
| Owns | The ledger schema and its migrations; every read and write of ledger data; the vocabulary and the storage triggers that enforce it; the identity format; the audit trail |
| Depends on | `modernc.org/sqlite` (a pure Go SQLite driver) and `github.com/google/uuid` |
| Used by | `mcp-local`, through a thin adapter that supplies the calling MCP client's name; `ledger-server`, the web UI |

## Layout

| Path | What |
|---|---|
| `store.go` | `Open`, `Options`, the `Store` contract, and the migrations that run on open |
| `schema.sql` | The schema, embedded in the binary |
| `items.go`, `edit.go`, `move.go` | Work items: create, list, edit as one transaction with an optional conflict check, move between projects |
| `labels.go`, `assignees.go` | Labels, assignees and watchers: child lists, unique per live member, with their one-time copies of the old single-value columns |
| `people.go`, `members.go`, `identity.go` | Creator and owner, project members and their roles, the `kind:id` format |
| `projects.go`, `notes.go`, `resources.go`, `relations.go` | Projects and their key aliases, notes and timers, resources, relations between items |
| `workflows*.go`, `workflow_doc.go` | Versioned workflow documents linked to epics, stories and tickets |
| `audit.go`, `actor.go`, `delete.go`, `policy.go` | The audit trail, who is writing, soft delete and restore, the permission seam |
| `vocabulary.go`, `enforce.go` | The closed sets of values, and the triggers generated from them |
| `health.go`, `search.go`, `summary.go` | The ledger's own data checks, search, project summaries |
| `cmd/backfill-people` | One-off: fill creator and owner on records that predate those fields |
| `cmd/drop-legacy-label`, `cmd/drop-legacy-assignee` | One-off, gated: remove an old column after the zero-loss check passes |

## Build and test

```
go test ./...          # full suite, including the multi-process concurrency regression test
go test -short ./...   # skip the test that spawns child processes
```

The tests open real SQLite databases in temp directories through the public `Open()`. The
concurrency test re-executes the test binary as five separate writer processes, because SQLite's
cross-process locking is what is under test.

## Related

- The design record for the ledger lives in the `datum-core` workspace repository, in its
  `docs/ledger.md`.
- `mcp-local` exposes the ledger as MCP tools; `ledger-server` is the web UI.

## Using it

```go
import ledgercore "github.com/digital-michael/ledger-core"

store, err := ledgercore.Open(ledgercore.Options{
    // Path: "" means DefaultPath(): LEDGER_DB_PATH, else ~/.local/share/mcp-local/ledger.db
    Actor: func(ctx context.Context) ledgercore.Actor {
        return ledgercore.Actor{Kind: ledgercore.ActorProgram, ID: "my-program"} // recorded on every audit row
    },
})
```

`ledgercore.Vocab()` returns the valid item types, statuses, relation types, note types and member
roles, so a caller can offer only valid choices.

## Invariants

These hold because something went wrong once. Do not change them casually.

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
- **A list is a child table, a single value is a column.** Labels, assignees, watchers and project
  members are rows, unique per live member by a partial unique index, so a removed member can be
  added again. `created_by` is set once and has no edit path; `owner` changes only through an
  audited transfer.
- **A column that a child table replaces is copied once, then dropped by a separate gated step.**
  Opening the ledger copies each old value into its table only for records that have no row yet, so
  a removal is never undone. The drop command refuses unless every old value is represented.
- **The last owner of a project cannot be removed or demoted**, including through the generic soft
  delete.

## Releases

Published at `github.com/digital-michael/ledger-core` with semver tags (annotated, with a
one-paragraph summary). Consumers pin a tagged version in `go.mod`, so a build outside the
development workspace (another machine, CI, a container image) is reproducible.

Local development across the consumers uses a `go.work` file that is not committed: it builds them
against this checkout. A change here is therefore live locally before it is released, and **not** in
any standalone build until it is. After a change lands:

1. Tag a release: patch for fixes and behaviour corrections with no API change, minor for new API.
   Push the tag.
2. Bump the pin in each consumer (`GOWORK=off go get github.com/digital-michael/ledger-core@vX.Y.Z`,
   then `GOWORK=off go mod tidy`), and confirm `GOWORK=off go build ./...` and the tests pass.
