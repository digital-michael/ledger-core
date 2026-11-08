// Command backfill-people fills created_by and owner on the ledger's existing items and projects, and gives every project without one a first owner.
//
// # Goals
//
//   - Fill the people fields from the first "created" audit row of each item
//     and project, using ledgercore.CreatorFromAudit, and report what it did.
//   - Report only by default; change the database only with -apply.
//   - Be safe to run again: only empty values are filled, so a second run
//     changes nothing.
//
// # Non-goals
//
//   - It never overwrites a value that is already set.
//   - It does not guess: with no audit record the creator is the person given
//     with -owner.
//   - It does not back up the database; take a backup first.
//
// # Boundaries
//
// Owns: the one-off decision of who created and who owns the records that predate those fields.
// Depends on: github.com/digital-michael/ledger-core.
// Used by: the operator, by hand, at a cut-over.
package main
