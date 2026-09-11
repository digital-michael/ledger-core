package ledgercore

import (
	"os"
	"strings"
)

// Links into ledger-server, the ledger's web UI.
//
// This lives here, rather than in ledger-server, because two programs must
// agree on it: ledger-server serves these paths, and mcp-local prints them in
// tool output so a person can click straight to a ticket. ledger-core is the
// one module both use. It describes how a ticket is addressed, not how it is
// served.

// DefaultServerURL is where ledger-server listens unless LEDGER_SERVER_URL
// says otherwise.
const DefaultServerURL = "http://127.0.0.1:8090"

// ItemPath is the path prefix for one ticket's page; the full id follows it.
// A full id, not a short prefix: a prefix that is unique today can become
// ambiguous as the ledger grows, and these links outlive the chats they are
// written in. (ledger-server still accepts a unique prefix, and redirects.)
const ItemPath = "/t/"

// ServerURL returns LEDGER_SERVER_URL if set, else DefaultServerURL, without
// a trailing slash.
func ServerURL() string {
	if v := strings.TrimSpace(os.Getenv("LEDGER_SERVER_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultServerURL
}

// ItemURL is the link that opens one ticket in ledger-server.
func ItemURL(id string) string {
	return ServerURL() + ItemPath + id
}
