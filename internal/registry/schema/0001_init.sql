-- 0001 — the registry, its operations, the API tokens.
--
-- Every resource any plugin makes is a row here, whatever its type: the core
-- counts, lists, audits and reconciles them without knowing what they are.
-- A deleted resource keeps its row (state 'deleted', deleted_at set): the
-- registry is also the history of who held what.

CREATE TABLE resources (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,   -- the listing's cursor
  id         TEXT    NOT NULL UNIQUE,             -- box-0123456789abcdef0
  type       TEXT    NOT NULL,
  plugin     TEXT    NOT NULL,
  owner      TEXT    NOT NULL,                    -- the identity provider's subject
  zone       TEXT    NOT NULL,
  state      TEXT    NOT NULL CHECK (state IN
               ('creating', 'ready', 'updating', 'deleting', 'deleted', 'failed', 'lost')),
  spec       TEXT    NOT NULL,                    -- JSON: desired
  observed   TEXT    NOT NULL DEFAULT '{}',       -- JSON: as last reported
  choices    TEXT    NOT NULL DEFAULT '{}',       -- JSON: CHOICE dimension -> value
  drift      TEXT    NOT NULL DEFAULT '',         -- the last reconcile's word when not in sync
  created_at TEXT    NOT NULL,
  updated_at TEXT    NOT NULL,
  deleted_at TEXT
);
CREATE INDEX resources_owner ON resources (owner, state);
CREATE INDEX resources_type  ON resources (type, state);

CREATE TABLE tags (
  resource_id TEXT NOT NULL REFERENCES resources (id),
  key         TEXT NOT NULL,
  value       TEXT NOT NULL,
  PRIMARY KEY (resource_id, key)
);
CREATE INDEX tags_key_value ON tags (key, value);

-- What each resource holds, per QUANTITY dimension. A tier's usage is the sum
-- over its owner's live resources (every state but deleted and failed).
CREATE TABLE usage (
  resource_id TEXT    NOT NULL REFERENCES resources (id),
  dimension   TEXT    NOT NULL,
  amount      INTEGER NOT NULL CHECK (amount >= 0),
  PRIMARY KEY (resource_id, dimension)
);

-- One resource's link to another: a volume attached_to a machine, a machine
-- born_from an image.
CREATE TABLE relations (
  from_id TEXT NOT NULL REFERENCES resources (id),
  kind    TEXT NOT NULL,
  to_id   TEXT NOT NULL REFERENCES resources (id),
  PRIMARY KEY (from_id, kind, to_id)
);

-- A long action, as the person polls it. A 'running' row at start-up is an
-- operation the brain died during: it is run again (plugins are idempotent).
CREATE TABLE operations (
  seq          INTEGER PRIMARY KEY AUTOINCREMENT,
  id           TEXT    NOT NULL UNIQUE,           -- op-…
  owner        TEXT    NOT NULL,                  -- who asked
  resource_id  TEXT    NOT NULL REFERENCES resources (id),
  kind         TEXT    NOT NULL CHECK (kind IN ('create', 'delete', 'action')),
  action       TEXT    NOT NULL DEFAULT '',
  params       TEXT    NOT NULL DEFAULT '{}',
  client_token TEXT,                              -- AWS's ClientToken: a retry is not a second request
  request_hash TEXT    NOT NULL DEFAULT '',       -- the request a client token was first used for
  state        TEXT    NOT NULL CHECK (state IN ('running', 'succeeded', 'failed')),
  error        TEXT    NOT NULL DEFAULT '',
  result       TEXT    NOT NULL DEFAULT 'null',
  attempts     INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT    NOT NULL,
  updated_at   TEXT    NOT NULL,
  finished_at  TEXT,
  UNIQUE (owner, client_token)
);
CREATE INDEX operations_state    ON operations (state);
CREATE INDEX operations_resource ON operations (resource_id);

-- API tokens for automation. Only the secret's hash is kept; the groups are
-- the owner's when it was made, and a token lives no longer than max_ttl.
CREATE TABLE tokens (
  id         TEXT PRIMARY KEY,                    -- tok-…
  hash       TEXT NOT NULL UNIQUE,                -- sha256 of the secret, hex
  owner      TEXT NOT NULL,
  name       TEXT NOT NULL,
  groups     TEXT NOT NULL,                       -- JSON array
  scopes     TEXT NOT NULL,                       -- JSON array: "read", "write"
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  last_used  TEXT,
  revoked_at TEXT
);
CREATE INDEX tokens_owner ON tokens (owner);
