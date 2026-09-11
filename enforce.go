package ledgercore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// Storage-level vocabulary enforcement.
//
// Validation in this package protects every write that goes through it. These
// triggers protect the data from writes that do not: the sqlite3 CLI, a
// future tool, a script. They are generated from vocabulary.go at open time,
// so the database and the Go code enforce the same sets by construction.
//
// Triggers, not CHECK constraints, deliberately. SQLite cannot add a CHECK to
// an existing table without rebuilding it (create, copy, drop, rename) --
// and items is referenced by notes, resources, item_relations and itself, so
// that rebuild would be the riskiest migration this database could run. A
// trigger is added with no rebuild and no data rewritten.
//
// Triggers check only NEW writes. That is also deliberate: rows that predate
// enforcement (four off-vocabulary item_relations rows existed when this was
// written) are left alone for an explicit human decision, and because the
// UPDATE triggers fire only on the enforced column ("UPDATE OF col"), those
// rows can still be soft-deleted or restored -- which only touch deleted_at.

// vocabRule is one enforced column.
type vocabRule struct {
	table, column string
	values        []string
}

func vocabRules() []vocabRule {
	return []vocabRule{
		{"items", "type", itemTypes},
		{"items", "status", statuses},
		{"item_relations", "relation_type", relationTypes},
		{"notes", "type", noteTypes},
	}
}

// triggerPrefix is shared by every version of one rule's triggers, so stale
// versions can be found and dropped when the vocabulary changes.
func (r vocabRule) triggerPrefix() string {
	return "ledger_vocab_" + r.table + "_" + r.column + "_"
}

// suffix is a short hash of the value set. A vocabulary change produces a new
// trigger name, which is how an already-installed database learns it is out
// of date -- CREATE TRIGGER IF NOT EXISTS alone would keep the old set forever
// and reject every newly added value.
func (r vocabRule) suffix() string {
	sum := sha256.Sum256([]byte(strings.Join(r.values, "\x00")))
	return hex.EncodeToString(sum[:4])
}

func (r vocabRule) triggerSQL() map[string]string {
	quoted := make([]string, len(r.values))
	for i, v := range r.values {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	in := strings.Join(quoted, ", ")
	msg := strings.ReplaceAll(fmt.Sprintf("ledger vocabulary: %s.%s must be one of %s",
		r.table, r.column, strings.Join(r.values, ", ")), "'", "''")

	ins := r.triggerPrefix() + "ins_" + r.suffix()
	upd := r.triggerPrefix() + "upd_" + r.suffix()
	return map[string]string{
		ins: fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s BEFORE INSERT ON %s
WHEN NEW.%s NOT IN (%s)
BEGIN SELECT RAISE(ABORT, '%s'); END`, ins, r.table, r.column, in, msg),
		upd: fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s BEFORE UPDATE OF %s ON %s
WHEN NEW.%s NOT IN (%s)
BEGIN SELECT RAISE(ABORT, '%s'); END`, upd, r.column, r.table, r.column, in, msg),
	}
}

// ensureVocabTriggers installs the current triggers and drops any stale
// version of them. A no-op read of sqlite_master on every open after the
// first; it writes only when a trigger is missing or the vocabulary changed.
// Safe to run from several processes at once: creation is IF NOT EXISTS, and
// only triggers whose name differs from the current one are dropped.
func ensureVocabTriggers(conn *sql.DB) error {
	existing := map[string]bool{}
	rows, err := conn.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'ledger\_vocab\_%' ESCAPE '\'`)
	if err != nil {
		return fmt.Errorf("listing vocabulary triggers: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}

	var stmts []string
	for _, r := range vocabRules() {
		want := r.triggerSQL()
		for name := range existing {
			if strings.HasPrefix(name, r.triggerPrefix()) && want[name] == "" {
				stmts = append(stmts, "DROP TRIGGER IF EXISTS "+name)
			}
		}
		for name, ddl := range want {
			if !existing[name] {
				stmts = append(stmts, ddl)
			}
		}
	}
	if len(stmts) == 0 {
		return nil
	}

	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("installing vocabulary trigger: %w", err)
		}
	}
	return tx.Commit()
}
