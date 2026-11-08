package ledgercore

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// The PostgreSQL backend: the same Store, over a postgres:// connection.
//
// There is no second copy of the SQL. The pgx driver is wrapped so that "?"
// placeholders and the group_concat idiom are translated on the way out (rebind.go),
// and what cannot be shared lives in pgDialect below. A stamp column is declared
// COLLATE "C" so that comparing two stamps is byte order whatever locale the server
// was initialised with.

// openPostgres connects to opts.DSN, applies the schema and the migrations, and
// returns the ready ledger.
func openPostgres(opts Options) (*DB, error) {
	cfg, err := pgx.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("reading the PostgreSQL connection string: %w", err)
	}
	raw := sql.OpenDB(newTranslatingConnector(stdlib.GetConnector(*cfg), postgresSQL))
	// A ledger is used by a handful of programs at once; a small pool is plenty and
	// keeps this one from crowding a shared server.
	raw.SetMaxOpenConns(10)
	if err := raw.Ping(); err != nil {
		raw.Close()
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	return initialize(&dbConn{DB: raw, d: pgDialect{}}, opts)
}

type pgDialect struct{}

func (pgDialect) name() string { return "postgres" }

func (pgDialect) tables(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (pgDialect) columns(ctx context.Context, q querier, table string) ([]columnInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.column_name,
		EXISTS (SELECT 1 FROM information_schema.table_constraints tc
		        JOIN information_schema.key_column_usage k
		          ON k.constraint_name = tc.constraint_name AND k.table_schema = tc.table_schema AND k.table_name = tc.table_name
		        WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = c.table_schema
		          AND tc.table_name = c.table_name AND k.column_name = c.column_name)
		FROM information_schema.columns c
		WHERE c.table_schema = current_schema() AND c.table_name = ? ORDER BY c.ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s schema: %w", table, err)
	}
	defer rows.Close()
	var out []columnInfo
	for rows.Next() {
		var ci columnInfo
		if err := rows.Scan(&ci.Name, &ci.PK); err != nil {
			return nil, err
		}
		out = append(out, ci)
	}
	return out, rows.Err()
}

// stampRegexp is the PostgreSQL spelling of stampGlob.
const stampRegexp = `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{9}Z$`

func (pgDialect) notStamp(col string) string { return fmt.Sprintf("%s !~ '%s'", col, stampRegexp) }

func (pgDialect) insertIgnore(table string, columns ...string) string {
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING", table, strings.Join(columns, ", "), placeholders(len(columns)))
}

func (pgDialect) columnDef(column, typ string) string {
	if typ == "TEXT" && strings.HasSuffix(column, "_at") {
		return `TEXT COLLATE "C"`
	}
	return typ
}

var stampColumnRE = regexp.MustCompile(`(?m)^(\s+\w+_at\s+)TEXT\b`)

// postgresSchema turns schema.sql into the list of statements PostgreSQL runs: stamp
// columns become byte-ordered, comments go, and the script is split, because the
// extended protocol takes one statement at a time.
func postgresSchema(schema string) []string {
	schema = stampColumnRE.ReplaceAllString(schema, `${1}TEXT COLLATE "C"`)
	var kept []string
	for _, line := range strings.Split(schema, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		kept = append(kept, line)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (pgDialect) applySchema(ctx context.Context, c *dbConn, schema string) error {
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range postgresSchema(schema) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w (in: %s)", err, firstLine(stmt))
		}
	}
	return tx.Commit()
}

// initLockSQL takes a session-level advisory lock named for the schema, so two
// ledgers in one database do not wait for each other but two openers of one do.
const (
	initLockSQL   = `SELECT pg_advisory_lock(hashtext(current_schema() || ':ledger-init'))`
	initUnlockSQL = `SELECT pg_advisory_unlock(hashtext(current_schema() || ':ledger-init'))`
)

// lockInit pins one connection for the lock, because a session lock belongs to the
// connection that took it. Opening applies the schema and runs the migrations, and
// PostgreSQL's CREATE ... IF NOT EXISTS is not safe to race: five programs opening an
// empty ledger together failed with duplicate-key errors and a deadlock before this.
func (pgDialect) lockInit(ctx context.Context, c *dbConn) (func(), error) {
	pinned, err := c.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := pinned.ExecContext(ctx, initLockSQL); err != nil {
		pinned.Close()
		return nil, fmt.Errorf("taking the initialisation lock: %w", err)
	}
	return func() {
		pinned.ExecContext(context.Background(), initUnlockSQL)
		pinned.Close()
	}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ensureVocab installs the vocabulary rules as trigger functions, from the same lists
// the SQLite triggers are generated from, and drops stale versions of them. A rule
// is a function and a pair of triggers sharing one name; the name carries a hash of
// the value set, so a changed vocabulary makes a new name and the old one is dropped.
func (pgDialect) ensureVocab(ctx context.Context, c *dbConn) error {
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	existing := map[string]string{} // trigger name -> table
	rows, err := tx.QueryContext(ctx, `SELECT t.tgname, cl.relname FROM pg_trigger t
		JOIN pg_class cl ON cl.oid = t.tgrelid JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE n.nspname = current_schema() AND NOT t.tgisinternal AND t.tgname LIKE 'ledger\_vocab\_%' ESCAPE '\'`)
	if err != nil {
		return fmt.Errorf("listing vocabulary triggers: %w", err)
	}
	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			rows.Close()
			return err
		}
		existing[name] = table
	}
	rows.Close()

	wanted := map[string]bool{}
	var stmts []string
	for _, r := range vocabRules() {
		ins := r.triggerPrefix() + "ins_" + r.suffix()
		upd := r.triggerPrefix() + "upd_" + r.suffix()
		wanted[ins], wanted[upd] = true, true
		for name, when := range map[string]string{ins: "INSERT", upd: "UPDATE OF " + r.column} {
			if _, ok := existing[name]; ok {
				continue
			}
			stmts = append(stmts, pgTriggerFunction(name, r), fmt.Sprintf(
				`CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, when, r.table, name))
		}
	}
	for name, table := range existing {
		if !wanted[name] {
			stmts = append(stmts, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", name, table), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", name))
		}
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("installing vocabulary trigger: %w", err)
		}
	}
	return tx.Commit()
}

// pgTriggerFunction is the body of one rule: refuse a value outside the set, with the
// same wording as the SQLite trigger.
func pgTriggerFunction(name string, r vocabRule) string {
	quoted := make([]string, len(r.values))
	for i, v := range r.values {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	msg := fmt.Sprintf("ledger vocabulary: %s.%s must be one of %s", r.table, r.column, strings.Join(r.values, ", "))
	return fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $body$
BEGIN
  IF NEW.%s NOT IN (%s) THEN
    RAISE EXCEPTION '%s' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$body$`, name, r.column, strings.Join(quoted, ", "), strings.ReplaceAll(msg, "'", "''"))
}
