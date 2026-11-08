// Command copy-ledger copies projects (or everything) from one ledger into another,
// SQLite to PostgreSQL or either way round, keeping every id.
//
// # Goals
//
//   - Copy whole projects, with their tickets, labels, assignees, watchers, notes,
//     resources, relations, members, workflows and the audit trail of all of them. A
//     plan or a workflow is copied as the whole projects it belongs to and points at.
//   - Stop and name the other projects whenever the selection is linked to projects that
//     were not chosen, and let the operator include named ones, leave every outside link
//     out (and say so), or cancel. Nothing is dropped or followed silently.
//   - Report only by default; change the target only with -apply. A dry run is the real
//     copy rolled back, so it meets every constraint and vocabulary rule the real one would.
//   - Be safe to repeat: a row already on the target and identical is counted, not written;
//     a row that differs is a conflict and stops the whole copy, which is all or nothing.
//   - Confirm a copy on request (-verify): re-read both ledgers and compare every selected
//     row, alone (writes nothing) or right after -apply, reporting what is missing, what
//     differs, and what exists only on the target.
//   - List every row whose value the ledger's current vocabulary no longer allows (left over
//     from before the rules existed) instead of failing on the first; leaf rows (relations,
//     notes, workflow links, members) may be left out with -skip-invalid and are named in the
//     report; a ticket or workflow is never skipped (exit status 2 when a decision is needed).
//   - Read a SQLite source from a snapshot, so the source ledger is never written to.
//
// # Non-goals
//
//   - It does not move: nothing is deleted from the source.
//   - It does not merge or overwrite: an existing different row is a conflict, not an update
//     (the one exception, a parent link an earlier copy had to leave out, is filled in and
//     counted).
//   - It does not provision the target database, back up either side, or translate
//     identities unless told to (-map-identity). Audit entries keep their original detail text.
//
// # Boundaries
//
// Owns: choosing, reading, writing and verifying a copy between two ledgers.
// Depends on: github.com/digital-michael/ledger-core (ledgercore.CopyLedger).
// Used by: the operator, by hand, to rehearse and perform a move to the remote ledger.
package main
