-- 0004 — schedules: whether a new request may name a resource yet, and
-- where each schedule of the operator's stands (ARCHITECTURE.md §3).

-- The plugin's say: why a new request may not name the resource, in one
-- word ('' = it may — a machine born from an image), and whether it is still
-- being made (it may become usable by itself).
ALTER TABLE resources ADD COLUMN unusable TEXT    NOT NULL DEFAULT '';
ALTER TABLE resources ADD COLUMN pending  INTEGER NOT NULL DEFAULT 0;

-- A schedule's clock, kept across restarts: since when it counts (its first
-- sight, or its last run), and what its last run did. The schedules
-- themselves are the operator's file; a row whose name left the file is
-- kept, and read again if the name comes back.
CREATE TABLE schedules (
  name          TEXT PRIMARY KEY,
  since         TEXT NOT NULL,  -- the time its next run is counted from
  last_run      TEXT,           -- when it last came (NULL: never yet)
  last_result   TEXT NOT NULL DEFAULT '',  -- asked, skipped, refused
  last_detail   TEXT NOT NULL DEFAULT '',
  last_resource TEXT NOT NULL DEFAULT ''
);
