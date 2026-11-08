// Command drop-legacy-assignee removes the old items.assignee column once every assignee is safely in item_assignees.
//
// # Goals
//
//   - Print the zero-loss check, per project, before anything changes, and
//     list any legacy value that could not be mapped to a person or agent.
//   - Drop the column only with -apply, and only if the check passes.
//   - Be safe to run twice: once the column is gone it reports that and does
//     nothing.
//
// # Non-goals
//
//   - It does not guess what an unrecognised assignee value meant; a person
//     decides.
//   - It does not back up the database; take a backup first.
//   - It does not check that other programs have stopped reading the column.
//
// # Boundaries
//
// Owns: the one irreversible step of the assignee migration.
// Depends on: github.com/digital-michael/ledger-core.
// Used by: the operator, by hand, at a cut-over.
package main
