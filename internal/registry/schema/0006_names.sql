-- 0006 — names: what a person calls a resource, and what the brain calls a
-- person (ARCHITECTURE.md §3 "Names").

-- A name of its owner's and a line of description, on every type: the brain's
-- own, never a spec's. '' = unnamed (it is shown by its id). The id stays the
-- identity: it never changes and is never used again.
ALTER TABLE resources ADD COLUMN name        TEXT NOT NULL DEFAULT '';
ALTER TABLE resources ADD COLUMN description TEXT NOT NULL DEFAULT '';

-- What was held before names: the entry a spec file made it from (the
-- command line's tag apply:name) first, else the name its spec carried (a
-- machine's, an image's).
UPDATE resources SET name = COALESCE(
  (SELECT value FROM tags WHERE resource_id = resources.id AND key = 'apply:name'),
  CASE WHEN json_type(spec, '$.name') = 'text' THEN json_extract(spec, '$.name') END,
  '');
-- One that is not a name (a-z, 0-9 and "-", at most 63, neither end a "-")
-- is left unnamed.
UPDATE resources SET name = '' WHERE name != ''
  AND (length(name) > 63 OR name GLOB '*[^a-z0-9-]*' OR name GLOB '-*' OR name GLOB '*-');
-- Among one owner's live resources of one type a name is one thing: the
-- oldest keeps it.
UPDATE resources SET name = '' WHERE name != ''
  AND state IN ('creating', 'ready', 'updating', 'deleting', 'lost')
  AND EXISTS (SELECT 1 FROM resources o
              WHERE o.owner = resources.owner AND o.type = resources.type AND o.name = resources.name
                AND o.state IN ('creating', 'ready', 'updating', 'deleting', 'lost') AND o.seq < resources.seq);
-- A spec names nothing any more. What a machine was born as on its engine
-- (its host name) stays what it is there.
UPDATE resources SET spec = json_remove(spec, '$.name') WHERE json_type(spec, '$.name') IS NOT NULL;

CREATE UNIQUE INDEX resources_name ON resources (owner, type, name)
  WHERE name != '' AND state IN ('creating', 'ready', 'updating', 'deleting', 'lost');

-- The name each subject last signed in under at the identity provider: an
-- owner is shown by it, never by the subject alone. A token carries its own
-- name, not its owner's, and writes nothing here.
CREATE TABLE subjects (
  subject TEXT PRIMARY KEY,
  name    TEXT NOT NULL,
  seen_at TEXT NOT NULL
);
CREATE INDEX subjects_name ON subjects (name);
