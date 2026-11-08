#!/bin/bash
# smoke-copy.sh: pass/fail smoke test of copy-ledger (SQLite to PostgreSQL).
#
#   scripts/smoke-copy.sh [path-to-ledger.db]     # default: the live ledger's usual location
#   SMOKE_ALL=1 scripts/smoke-copy.sh             # also rehearse copying every live project
#
# It takes a snapshot of the ledger into a scratch directory, builds a temporary project in the
# SNAPSHOT (never in the ledger itself), copies it into a throwaway PostgreSQL (loopback only, in
# memory), verifies the copy row for row, then links the temporary project to a real one to
# exercise the outside-link decision. Nothing real is written; the server and the scratch copy are
# removed at the end. The exit status is the test's.

set -u

LIVE="${1:-${LEDGER_DB_PATH:-$HOME/.local/share/mcp-local/ledger.db}}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"

if [ ! -f "$LIVE" ]; then
	echo "smoke-copy: no ledger at $LIVE" >&2
	exit 1
fi
command -v sqlite3 >/dev/null || { echo "smoke-copy: sqlite3 is needed to take the snapshot" >&2; exit 1; }

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/smoke-copy.XXXXXX")" || exit 1
cleanup() {
	( cd "$HERE" && make --no-print-directory pg-down ) >/dev/null 2>&1
	rm -f "$SCRATCH/ledger.db" "$SCRATCH/ledger.db-wal" "$SCRATCH/ledger.db-shm"
	rmdir "$SCRATCH" 2>/dev/null
}
trap cleanup EXIT

echo "snapshotting $LIVE"
sqlite3 "$LIVE" ".backup '$SCRATCH/ledger.db'" || { echo "smoke-copy: the snapshot failed" >&2; exit 1; }

( cd "$HERE" && make --no-print-directory pg-up ) || exit 1

cd "$HERE" || exit 1
LEDGERCORE_SMOKE_SOURCE="$SCRATCH/ledger.db" \
LEDGERCORE_SMOKE_ALL="${SMOKE_ALL:-}" \
LEDGERCORE_TEST_PG='postgres://ledger:ledgertest@127.0.0.1:54329/ledger_test?sslmode=disable' \
	go test -count=1 -run 'TestSmoke_' -v .
status=$?

if [ "$status" -eq 0 ]; then
	echo "SMOKE PASSED"
else
	echo "SMOKE FAILED" >&2
fi
exit "$status"
