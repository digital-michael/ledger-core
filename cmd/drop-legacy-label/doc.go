// Command drop-legacy-label removes the old items.label column once every label is safely in item_labels.
//
// # Goals
//
//   - Print the zero-loss check, per project, before anything changes.
//   - Drop the column only with -apply, and only if the check passes.
//   - Be safe to run twice: once the column is gone it reports that and does
//     nothing.
//
// # Non-goals
//
//   - It does not copy labels; opening the ledger does that.
//   - It does not back up the database; take a backup first.
//   - It does not check that other programs have stopped reading the column.
//
// # Boundaries
//
// Owns: the one irreversible step of the label migration.
// Depends on: github.com/digital-michael/ledger-core.
// Used by: the operator, by hand, at a cut-over.
package main
