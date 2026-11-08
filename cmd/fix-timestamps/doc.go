// Command fix-timestamps rewrites every stored timestamp in the ledger into the
// fixed-width UTC form, so that sorting them as text sorts them by time.
//
// # Goals
//
//   - Report, per table and column, how many stamps are already fixed width and how
//     many would be rewritten, and change nothing without -apply.
//   - Rewrite each value to the same instant (an offset is converted to UTC), in one
//     transaction, and verify before committing that every value is fixed width and
//     that none appeared or went missing.
//   - Be safe to run again: a ledger that is already fixed width reports zero changes.
//
// # Non-goals
//
//   - It does not run on open; it is an operator command.
//   - It does not guess: if any value cannot be read as a timestamp it changes
//     nothing and names the value.
//   - It does not back up the database; take a backup first.
//
// # Boundaries
//
// Owns: the one-off conversion of stamps written before the fixed-width format.
// Depends on: github.com/digital-michael/ledger-core.
// Used by: the operator, by hand, once per ledger (and again if a program still on an
// older release wrote stamps in the meantime).
package main
