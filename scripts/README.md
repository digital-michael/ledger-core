# scripts

Shell scripts that check the ledger tools against real data without touching it.

## Goals

- Give a pass or fail answer to "does the copy tool work on a ledger like the real one", in one
  command, so the check is not repeated by hand.
- Work on a snapshot of the ledger and a throwaway PostgreSQL, so the real ledger is never written.

## Non-goals

- They do not change the ledger, install anything, or leave a server or scratch file behind.
- They do not replace the unit tests (`make test`, `make test-pg`); they run the same code against
  real data.

## Boundaries

Owns: the wiring of a snapshot, a throwaway server and the gated smoke tests.
Depends on: `sqlite3`, `podman` (through `make pg-up`), Go, and the tests in `smoke_test.go`.
Used by: the operator, before a real copy or move of a ledger.

## Contents

| File | What it does and when to run it |
|---|---|
| `smoke-copy.sh` | Snapshots the ledger, builds a temporary project in the snapshot, copies it to a throwaway PostgreSQL, verifies it row for row, then links it to a real project to test the outside-link decision. `SMOKE_ALL=1` also copies every live project. Run it before trusting `copy-ledger` with a real ledger. |
