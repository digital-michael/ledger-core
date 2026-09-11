package ledgercore

import "time"

// nowUTC returns the current time as an RFC3339Nano string, the format used
// for every created_at/updated_at column in this schema.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// nullIfEmpty maps an empty string to a SQL NULL, and anything else through
// unchanged. Used for optional TEXT columns bound via database/sql.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
