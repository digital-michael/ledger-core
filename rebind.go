package ledgercore

import (
	"context"
	"database/sql/driver"
	"regexp"
	"strconv"
	"strings"
)

// The PostgreSQL backend does not get its own copy of the SQL. This package writes
// every statement once, in the form it has always used -- "?" placeholders and a few
// SQLite spellings -- and a thin driver wrapper translates it on its way to the
// server. About 150 call sites stay exactly as they are, and a statement cannot
// drift between the two databases because there is only one.
//
// What the translation does is deliberately small:
//
//   - "?" becomes "$1", "$2", ... in order, but never inside quoted text, a quoted
//     identifier, a comment, or a dollar-quoted body;
//   - the two idioms this package repeats, group_concat(x, char(31)) and nothing
//     else, are spelled the PostgreSQL way (string_agg(x, chr(31))).
//
// Anything that differs structurally (listing tables, triggers, upserts) is not
// rewritten here: it lives behind the dialect seam in dialect.go.

// rewriteCode applies f to every stretch of q that is real SQL, leaving string
// literals, quoted identifiers, comments and dollar-quoted bodies untouched. f is
// called in order, so it may keep state (the placeholder counter).
func rewriteCode(q string, f func(code string) string) string {
	var out strings.Builder
	start := 0 // start of the current code stretch
	flush := func(end int) {
		if end > start {
			out.WriteString(f(q[start:end]))
		}
	}
	i := 0
	for i < len(q) {
		c := q[i]
		var end int // index just past the protected region, or 0 if none starts here
		switch {
		case c == '\'':
			end = skipQuoted(q, i, '\'')
		case c == '"':
			end = skipQuoted(q, i, '"')
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			end = strings.IndexByte(q[i:], '\n')
			if end < 0 {
				end = len(q)
			} else {
				end += i
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			end = strings.Index(q[i+2:], "*/")
			if end < 0 {
				end = len(q)
			} else {
				end += i + 4
			}
		case c == '$':
			if tag := dollarTag(q[i:]); tag != "" {
				if j := strings.Index(q[i+len(tag):], tag); j >= 0 {
					end = i + len(tag) + j + len(tag)
				} else {
					end = len(q)
				}
			}
		}
		if end == 0 {
			i++
			continue
		}
		flush(i)
		out.WriteString(q[i:end])
		i = end
		start = i
	}
	flush(len(q))
	return out.String()
}

// skipQuoted returns the index just past the quoted run that opens at q[i], where
// a doubled quote character inside it is an escaped quote.
func skipQuoted(q string, i int, quote byte) int {
	for j := i + 1; j < len(q); j++ {
		if q[j] == quote {
			if j+1 < len(q) && q[j+1] == quote {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(q)
}

var dollarTagRE = regexp.MustCompile(`^\$[A-Za-z_]*\$`)

// dollarTag returns the opening tag ("$$" or "$body$") of a dollar-quoted string at
// the start of s, or "". "$1" is a parameter, not a tag.
func dollarTag(s string) string { return dollarTagRE.FindString(s) }

// rebind rewrites "?" placeholders to "$1", "$2", ... SQL that has no "?" (already
// numbered, or parameterless) comes back unchanged.
func rebind(q string) string {
	if !strings.Contains(q, "?") {
		return q
	}
	n := 0
	return rewriteCode(q, func(code string) string {
		if !strings.Contains(code, "?") {
			return code
		}
		var b strings.Builder
		for i := 0; i < len(code); i++ {
			if code[i] == '?' {
				n++
				b.WriteString("$" + strconv.Itoa(n))
			} else {
				b.WriteByte(code[i])
			}
		}
		return b.String()
	})
}

var (
	groupConcatRE = regexp.MustCompile(`(?i)\bgroup_concat\s*\(`)
	char31RE      = regexp.MustCompile(`(?i)\bchar\(31\)`)
)

// postgresSQL is the whole translation for the PostgreSQL driver: the spellings,
// then the placeholders.
func postgresSQL(q string) string {
	spelled := rewriteCode(q, func(code string) string {
		code = groupConcatRE.ReplaceAllString(code, "string_agg(")
		return char31RE.ReplaceAllString(code, "chr(31)")
	})
	return rebind(spelled)
}

// ---- the driver wrapper ----

// translatingConnector wraps another driver.Connector and passes every statement
// through translate on its way in. It forwards every optional interface the inner
// connection offers, so the inner driver (pgx) behaves exactly as it would alone.
type translatingConnector struct {
	inner     driver.Connector
	translate func(string) string
}

func newTranslatingConnector(inner driver.Connector, translate func(string) string) driver.Connector {
	return &translatingConnector{inner: inner, translate: translate}
}

func (c *translatingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &translatingConn{Conn: conn, translate: c.translate}, nil
}

func (c *translatingConnector) Driver() driver.Driver { return c.inner.Driver() }

type translatingConn struct {
	driver.Conn
	translate func(string) string
}

func (c *translatingConn) Prepare(q string) (driver.Stmt, error) {
	return c.Conn.Prepare(c.translate(q))
}

func (c *translatingConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, c.translate(q))
	}
	return c.Conn.Prepare(c.translate(q))
}

// ExecContext and QueryContext return driver.ErrSkip when the inner connection has no
// fast path, which makes database/sql fall back to PrepareContext (translated above).
func (c *translatingConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, c.translate(q), args)
	}
	return nil, driver.ErrSkip
}

func (c *translatingConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if e, ok := c.Conn.(driver.QueryerContext); ok {
		return e.QueryContext(ctx, c.translate(q), args)
	}
	return nil, driver.ErrSkip
}

func (c *translatingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin() //nolint:staticcheck // the inner driver only has the old form
}

func (c *translatingConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *translatingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *translatingConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *translatingConn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}
