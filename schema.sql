-- Ledger schema. Applied on every Open() via CREATE TABLE IF NOT EXISTS,
-- which only handles brand-new tables — adding a column to an already-created
-- database is handled separately, by store.go's ensureColumn() migration
-- primitive (SQLite's ALTER TABLE ADD COLUMN has no "IF NOT EXISTS" form).
--
-- deleted_at (TEXT, nullable): soft-delete marker on every entity except
-- audit_log (the append-only record of what happened, including deletes,
-- so it is not itself deletable). NULL means not deleted. No cascade —
-- deleting a project or item does not soft-delete its children.

CREATE TABLE IF NOT EXISTS projects (
  id         TEXT PRIMARY KEY,
  key        TEXT UNIQUE NOT NULL,
  name       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  deleted_at TEXT
);

CREATE TABLE IF NOT EXISTS items (
  id           TEXT PRIMARY KEY,
  project_id   TEXT NOT NULL REFERENCES projects(id),
  parent_id    TEXT REFERENCES items(id),
  type         TEXT NOT NULL DEFAULT 'task',
  title        TEXT NOT NULL,
  description  TEXT,
  status       TEXT NOT NULL,
  label        TEXT,
  priority     INTEGER,
  order_key    INTEGER,
  assignee     TEXT,
  component_id TEXT REFERENCES items(id),
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  deleted_at   TEXT
);

CREATE TABLE IF NOT EXISTS resources (
  id         TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  item_id    TEXT REFERENCES items(id),
  url        TEXT NOT NULL,
  label      TEXT,
  created_at TEXT NOT NULL,
  deleted_at TEXT
);

CREATE TABLE IF NOT EXISTS notes (
  id         TEXT PRIMARY KEY,
  project_id TEXT REFERENCES projects(id),
  item_id    TEXT REFERENCES items(id),
  type       TEXT NOT NULL DEFAULT 'comment',
  body       TEXT,
  url        TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  deleted_at TEXT
);

CREATE TABLE IF NOT EXISTS item_relations (
  id            TEXT PRIMARY KEY,
  from_item_id  TEXT NOT NULL REFERENCES items(id),
  to_item_id    TEXT NOT NULL REFERENCES items(id),
  relation_type TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  deleted_at    TEXT
);

CREATE TABLE IF NOT EXISTS audit_log (
  id          TEXT PRIMARY KEY,
  entity_type TEXT NOT NULL,
  entity_id   TEXT,
  operation   TEXT NOT NULL,
  detail      TEXT,
  created_at  TEXT NOT NULL,
  client      TEXT
);
-- client: the negotiated MCP ClientInfo.Name of whatever connection performed
-- this mutation (e.g. "mcp-console/run", "mcp-console/script_file",
-- "mcp-console/repl", or a live Claude Code session's own identity). NULL
-- when there's no session at all — e.g. mcp-local's own in-process -run,
-- which bypasses the real protocol entirely and has no client to name.
-- Populated automatically by insertAudit() from context; no caller passes it.

CREATE INDEX IF NOT EXISTS idx_items_project ON items(project_id);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_id);
-- idx_items_component intentionally NOT here: on a pre-existing database
-- this whole file is applied (CREATE TABLE IF NOT EXISTS is a no-op, but
-- CREATE INDEX still runs) BEFORE store.go's ensureColumn() migration adds
-- component_id, so an index on that column here would fail with "no such
-- column" on any database that predates it. Created in store.go's open(),
-- after the migration, instead.
CREATE INDEX IF NOT EXISTS idx_resources_project ON resources(project_id);
CREATE INDEX IF NOT EXISTS idx_resources_item ON resources(item_id);
CREATE INDEX IF NOT EXISTS idx_notes_item ON notes(item_id);
CREATE INDEX IF NOT EXISTS idx_notes_project ON notes(project_id);
CREATE INDEX IF NOT EXISTS idx_relations_from ON item_relations(from_item_id);
CREATE INDEX IF NOT EXISTS idx_relations_to ON item_relations(to_item_id);
CREATE INDEX IF NOT EXISTS idx_audit_entity ON audit_log(entity_type, entity_id);
