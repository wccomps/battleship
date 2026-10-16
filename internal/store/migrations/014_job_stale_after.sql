-- The stale_after of the worker running a job. Reapers from this version on
-- honour it, so the worker's lease on the job (which counts on it) holds
-- whatever stale_after they have. NULL (claimed by an older worker) falls
-- back to the reaper's own.
ALTER TABLE jobs ADD COLUMN stale_after interval;
