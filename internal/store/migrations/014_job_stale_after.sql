-- The stale_after of the worker running a job: the reaper honours it, so a
-- worker's lease on the job (which counts on it) holds whatever stale_after
-- the reaping replica has. NULL (claimed by an older worker) falls back to
-- the reaper's own.
ALTER TABLE jobs ADD COLUMN stale_after interval;
