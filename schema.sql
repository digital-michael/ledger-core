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
  id          TEXT PRIMARY KEY,
  project_id  TEXT NOT NULL REFERENCES projects(id),
  parent_id   TEXT REFERENCES items(id),
  type        TEXT NOT NULL DEFAULT 'task',
  title       TEXT NOT NULL,
  description TEXT,
  status      TEXT NOT NULL,
  label       TEXT,
  priority    INTEGER,
  order_key   INTEGER,
  assignee    TEXT,
  component   TEXT,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  deleted_at  TEXT
);
-- component: a plain denormalized string (an existing type=component item's
-- title, copied at assignment time), NOT a foreign key -- deliberately, so
-- project reorganization (merge/split, eventual cross-instance sync) never
-- needs to reconcile ids. Was a real component_id REFERENCES items(id)
-- column briefly; replaced before any external release existed to depend on
-- that shape. See docs/ledger.md for the full rationale.

CREATE TABLE IF NOT EXISTS resources (
  id         TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  item_id    TEXT REFERENCES items(id),
  url        TEXT NOT NULL,
  label      TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT,
  deleted_at TEXT
);
-- resources.updated_at: NULL means "never updated". Added 2026-09-10 with
-- UpdateResource; an existing database gets the column via store.go's
-- ensureColumn() and its existing rows are deliberately NOT backfilled --
-- a NULL is the honest value for a row nobody has edited, and adding the
-- column should not rewrite any data.

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

-- Workflows (added 2026-09-29): how a goal is achieved, as versioned
-- use-case documents linked to epics/stories and to the tickets that
-- implement or verify each step. See workflows.go.
--
-- workflows holds identity and lifecycle only; the content lives in
-- workflow_versions, one immutable row per saved version, as a validated
-- JSON document (WorkflowDoc). title is copied out of the document for
-- listing. The two link tables are what needs querying and integrity:
-- which epics/stories a workflow serves, and which tickets sit on which step.
CREATE TABLE IF NOT EXISTS workflows (
  id              TEXT PRIMARY KEY,
  project_id      TEXT NOT NULL REFERENCES projects(id),
  status          TEXT NOT NULL,
  current_version INTEGER NOT NULL,
  project_level   INTEGER NOT NULL DEFAULT 0,
  created_by      TEXT,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  deleted_at      TEXT
);

CREATE TABLE IF NOT EXISTS workflow_versions (
  id          TEXT PRIMARY KEY,
  workflow_id TEXT NOT NULL REFERENCES workflows(id),
  version     INTEGER NOT NULL,
  title       TEXT NOT NULL,
  document    TEXT NOT NULL,
  change_note TEXT,
  created_by  TEXT,
  created_at  TEXT NOT NULL,
  UNIQUE (workflow_id, version)
);
-- project_level: 1 for a project workflow -- an end-to-end journey or a
-- cross-cutting flow meant for the project as a whole, rather than for one
-- epic or story. Added 2026-09-30; store.go adds it to older databases.
-- workflow_versions has no deleted_at: versions are history, like audit_log.
-- Deleting a workflow soft-deletes the workflows row; its versions remain.

CREATE TABLE IF NOT EXISTS workflow_associations (
  id             TEXT PRIMARY KEY,
  workflow_id    TEXT NOT NULL REFERENCES workflows(id),
  item_id        TEXT NOT NULL REFERENCES items(id),
  pinned_version INTEGER,
  created_at     TEXT NOT NULL,
  deleted_at     TEXT
);
-- pinned_version: NULL follows the workflow's current version.

CREATE TABLE IF NOT EXISTS workflow_links (
  id          TEXT PRIMARY KEY,
  workflow_id TEXT NOT NULL REFERENCES workflows(id),
  part_key    TEXT NOT NULL,
  item_id     TEXT NOT NULL REFERENCES items(id),
  role        TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  deleted_at  TEXT
);
-- part_key: a stable key inside the workflow's documents (s3, s3.r1, a1 ...).
-- It survives edits; if a later version drops that part, the link is kept
-- and reported by HealthFindings rather than removed (docs/ledger.md U14).

CREATE INDEX IF NOT EXISTS idx_workflows_project ON workflows(project_id);
CREATE INDEX IF NOT EXISTS idx_workflow_versions_workflow ON workflow_versions(workflow_id);
CREATE INDEX IF NOT EXISTS idx_workflow_assoc_workflow ON workflow_associations(workflow_id);
CREATE INDEX IF NOT EXISTS idx_workflow_assoc_item ON workflow_associations(item_id);
CREATE INDEX IF NOT EXISTS idx_workflow_links_workflow ON workflow_links(workflow_id);
CREATE INDEX IF NOT EXISTS idx_workflow_links_item ON workflow_links(item_id);

CREATE INDEX IF NOT EXISTS idx_items_project ON items(project_id);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_id);
-- idx_items_component intentionally NOT here: on a pre-existing database
-- this whole file is applied (CREATE TABLE IF NOT EXISTS is a no-op, but
-- CREATE INDEX still runs) BEFORE store.go's ensureColumn() migration adds
-- component, so an index on that column here would fail with "no such
-- column" on any database that predates it. Created in store.go's open(),
-- after the migration, instead.
CREATE INDEX IF NOT EXISTS idx_resources_project ON resources(project_id);
CREATE INDEX IF NOT EXISTS idx_resources_item ON resources(item_id);
CREATE INDEX IF NOT EXISTS idx_notes_item ON notes(item_id);
CREATE INDEX IF NOT EXISTS idx_notes_project ON notes(project_id);
CREATE INDEX IF NOT EXISTS idx_relations_from ON item_relations(from_item_id);
CREATE INDEX IF NOT EXISTS idx_relations_to ON item_relations(to_item_id);
CREATE INDEX IF NOT EXISTS idx_audit_entity ON audit_log(entity_type, entity_id);
