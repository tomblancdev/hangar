-- 0002 — the room: what each resource takes from its zone's pools, the
-- holds the core places on borrowed room, and the claims a reservation's
-- guest makes before it starts (ARCHITECTURE.md §6).

-- MiB booked in the zone's guaranteed pool while the resource lives, MiB
-- borrowed from its spot pool while it runs, whether it is meant to run (as
-- the plugin planned it), and the key of the reservation holding its
-- borrowed room back ('' = none) since when.
ALTER TABLE resources ADD COLUMN room_guaranteed INTEGER NOT NULL DEFAULT 0 CHECK (room_guaranteed >= 0);
ALTER TABLE resources ADD COLUMN room_spot       INTEGER NOT NULL DEFAULT 0 CHECK (room_spot >= 0);
ALTER TABLE resources ADD COLUMN running         INTEGER NOT NULL DEFAULT 0;
ALTER TABLE resources ADD COLUMN hold            TEXT    NOT NULL DEFAULT '';
ALTER TABLE resources ADD COLUMN hold_since      TEXT;
CREATE INDEX resources_zone ON resources (zone, state);

-- A reservation in force on someone's word before its condition reads true:
-- its guest's hook claimed the room ('by' = who), or the engine's node was
-- seen holding for it. Released = the row deleted (the audit keeps the
-- history). One stays past the zone's grace only while its condition holds.
CREATE TABLE claims (
  zone        TEXT NOT NULL,
  reservation TEXT NOT NULL,
  by          TEXT NOT NULL,
  claimed_at  TEXT NOT NULL,
  PRIMARY KEY (zone, reservation)
);
