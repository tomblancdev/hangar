-- 0003 — shares: a resource its owner opened to others. A person in one of
-- a resource's groups (or anyone, for the group '*') sees it and may name it
-- in their own requests (a machine born from a shared image); only its owner
-- (or an operator) changes or deletes it. The rows follow the spec's field
-- the type's schema marks "x-hangar-share", as relations follow references.

CREATE TABLE shares (
  resource_id TEXT NOT NULL REFERENCES resources (id),
  grp         TEXT NOT NULL,                      -- a group's name, or '*' = everyone
  PRIMARY KEY (resource_id, grp)
);
CREATE INDEX shares_grp ON shares (grp);
