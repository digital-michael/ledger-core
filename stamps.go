package ledgercore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Timestamps are fixed-width UTC text: 2006-01-02T15:04:05.000000000Z, always 30
// characters, always nine fractional digits. Comparing two of them as text is then
// the same as comparing the moments they name, which is what every ORDER BY and
// every "created_at >= ?" in this package relies on.
//
// time.RFC3339Nano trims trailing zeros, so it cannot be used for that: it wrote
// "…:00Z" for a whole second and "…:00.5Z" for half a second past it, and as text
// the later one sorts first ('.' sorts before 'Z'). Stamps written before this
// format are rewritten, once and on request, by NormalizeTimestamps.

// stampLayout is the layout of every stamp this package writes. The fractional
// part is "000000000", not ".999999999", precisely so trailing zeros are kept.
const stampLayout = "2006-01-02T15:04:05.000000000Z"

// stampGlob matches exactly a fixed-width stamp, for SQLite's GLOB.
const stampGlob = "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z"

// FormatStamp formats t as a stamp, in UTC.
func FormatStamp(t time.Time) string { return t.UTC().Format(stampLayout) }

// ParseStamp reads a stamp, and any other RFC 3339 timestamp (older rows have no
// fixed width, and may carry an offset).
func ParseStamp(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// stampBound turns what a person typed for a lower bound -- a full RFC 3339
// timestamp with or without fractions or an offset, or just a date -- into a stamp
// that compares correctly as text against the stored ones.
func stampBound(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return FormatStamp(t), nil
		}
	}
	return "", fmt.Errorf("%q is not a timestamp: use RFC 3339 (2026-11-08T13:00:00Z) or a date (2026-11-08)", s)
}

// TimestampColumn is one column's line in a NormalizeTimestamps report.
type TimestampColumn struct {
	Table, Column string
	NonNull       int // values present
	Fixed         int // of those, already fixed width
	Rewritten     int // rewritten (or, in a dry run, that would be)
}

// TimestampReport is what NormalizeTimestamps did, or in a dry run would do.
type TimestampReport struct {
	Columns     []TimestampColumn
	Rewritten   int
	Unparseable []string // "table.column rowid: value" for anything that is not a timestamp
	Applied     bool
}

// timestampColumns lists every column named *_at in every table, so a table added
// later is covered without anyone remembering to list it.
func timestampColumns(ctx context.Context, tx *sql.Tx) ([][2]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, n)
	}
	rows.Close()
	var out [][2]string
	for _, tb := range tables {
		cols, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, tb))
		if err != nil {
			return nil, err
		}
		for cols.Next() {
			var cid, notnull, pk int
			var name, ctype string
			var dflt sql.NullString
			if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				cols.Close()
				return nil, err
			}
			if strings.HasSuffix(name, "_at") {
				out = append(out, [2]string{tb, name})
			}
		}
		cols.Close()
	}
	return out, nil
}

// NormalizeTimestamps rewrites every stored stamp that is not fixed-width into the
// fixed-width form, naming the same instant (an offset is converted to UTC). It is
// one transaction and all-or-nothing: if any value cannot be read as a timestamp,
// nothing is written and the value is reported. Without apply it only reports.
// Running it again changes nothing. Take a backup first; it is an operator command,
// not something that runs on open.
func (db *DB) NormalizeTimestamps(ctx context.Context, apply bool) (*TimestampReport, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	cols, err := timestampColumns(ctx, tx)
	if err != nil {
		return nil, err
	}
	rep := &TimestampReport{}
	type update struct {
		table, column string
		rowid         int64
		value         string
	}
	var updates []update
	before := map[string]int{}
	for _, c := range cols {
		tb, col := c[0], c[1]
		line := TimestampColumn{Table: tb, Column: col}
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL`, tb, col)).Scan(&line.NonNull); err != nil {
			return nil, err
		}
		before[tb+"."+col] = line.NonNull
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT rowid, %s FROM %s WHERE %s IS NOT NULL AND %s NOT GLOB '%s'`, col, tb, col, col, stampGlob))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var rowid int64
			var v string
			if err := rows.Scan(&rowid, &v); err != nil {
				rows.Close()
				return nil, err
			}
			t, perr := ParseStamp(v)
			if perr != nil {
				rep.Unparseable = append(rep.Unparseable, fmt.Sprintf("%s.%s rowid %d: %q", tb, col, rowid, v))
				continue
			}
			updates = append(updates, update{tb, col, rowid, FormatStamp(t)})
			line.Rewritten++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		line.Fixed = line.NonNull - line.Rewritten - countFor(rep.Unparseable, tb+"."+col)
		rep.Columns = append(rep.Columns, line)
		rep.Rewritten += line.Rewritten
	}
	sort.Strings(rep.Unparseable)
	if len(rep.Unparseable) > 0 {
		return rep, fmt.Errorf("%d value(s) cannot be read as a timestamp, so nothing was changed: %s", len(rep.Unparseable), strings.Join(rep.Unparseable, "; "))
	}
	if !apply {
		return rep, nil
	}

	for _, u := range updates {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, u.table, u.column), u.value, u.rowid); err != nil {
			return nil, fmt.Errorf("rewriting %s.%s: %w", u.table, u.column, err)
		}
	}
	// Verify before committing: every value is now fixed width, and no value
	// appeared or went missing.
	for _, c := range cols {
		tb, col := c[0], c[1]
		var bad, n int
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL AND %s NOT GLOB '%s'`, tb, col, col, stampGlob)).Scan(&bad); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL`, tb, col)).Scan(&n); err != nil {
			return nil, err
		}
		if bad != 0 || n != before[tb+"."+col] {
			return nil, fmt.Errorf("verification failed for %s.%s (%d not fixed width, %d values vs %d before): nothing was changed", tb, col, bad, n, before[tb+"."+col])
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	rep.Applied = true
	return rep, nil
}

func countFor(list []string, prefix string) int {
	n := 0
	for _, s := range list {
		if strings.HasPrefix(s, prefix+" ") {
			n++
		}
	}
	return n
}
