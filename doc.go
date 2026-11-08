// Package ledgercore is the ledger's persistence layer: one SQLite-backed
// implementation of every ledger read and write.
//
// It stores projects, work items (with labels, assignees, watchers, an owner and
// a creator), project members, resources, notes and timers, item relations,
// versioned workflows and an append-only audit log. Every program that touches
// the ledger database goes through this package, so validation, auditing, soft
// delete and id-prefix resolution cannot differ between front doors.
//
// # Goals
//
//   - Be the single implementation of every ledger write: vocabulary validation,
//     the audit row in the same transaction, soft delete, id-prefix resolution
//     and conflict detection on edits. This serves the platform goal of giving
//     models real tools (goal 3), because the ledger is one of those tool domains.
//   - Keep one identity format for every field that names a person or an agent
//     ("kind:id"), so a creator, an owner, an assignee and a project member mean
//     the same thing wherever they appear.
//   - Keep the ledger safe to share between several processes on one machine:
//     write-ahead logging, a busy timeout on every connection, and a regression
//     test that writes from separate processes.
//   - Offer one seam, Options.Permit, where access rules attach, so they are not
//     retrofitted across every write path.
//
// # Non-goals
//
//   - No transport. It knows nothing about MCP or HTTP; the caller says who is
//     writing through Options.Actor.
//   - No authentication and no user registry. It checks that an identity is
//     well formed, not that the person exists.
//   - No notification delivery. Watchers are stored; nothing is sent.
//   - No external synchronisation, and no database other than SQLite today.
//
// # Boundaries
//
// Owns: the ledger schema and its migrations; every read and write of ledger
// data; the vocabulary and the storage triggers that enforce it; the identity
// format; the audit trail.
// Depends on: modernc.org/sqlite (a pure Go SQLite driver) and
// github.com/google/uuid.
// Used by: mcp-local (through a thin adapter that supplies the calling MCP
// client's name) and ledger-server (the web UI).
package ledgercore
