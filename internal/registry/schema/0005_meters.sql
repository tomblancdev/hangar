-- 0005 — meters: what an owner CONSUMED, per METER dimension and calendar
-- month (ARCHITECTURE.md §3, §6 power) — the hours their machines ran.

-- One row per owner, dimension and month ('2026-10', in the brain's time
-- zone). It only grows: a delete gives nothing back, and a new month starts
-- with no row. A row is kept: it is the history of who used what.
CREATE TABLE meters (
  owner     TEXT NOT NULL,
  dimension TEXT NOT NULL,
  period    TEXT NOT NULL,                        -- the month: YYYY-MM
  used      REAL NOT NULL DEFAULT 0 CHECK (used >= 0),
  PRIMARY KEY (owner, dimension, period)
);

-- The tier a resource's last request was admitted under: the limits a
-- reconcile holds it to when nobody is asking (its owner's month spent).
-- '' = admitted before tiers were recorded: held to none until it is asked
-- for again.
ALTER TABLE resources ADD COLUMN tier TEXT NOT NULL DEFAULT '';
