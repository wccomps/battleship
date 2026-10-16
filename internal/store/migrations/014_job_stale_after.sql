-- The claiming worker's stale_after. Reapers from this version on honour it,
-- so its lease holds whatever theirs is; NULL (claimed by an older
-- battleship) falls back to the reaper's own.
ALTER TABLE jobs ADD COLUMN stale_after interval;
