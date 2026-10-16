-- lock_reads are what a job only reads (say, the base image it clones).
-- Jobs reading one VM run together; each waits only for a job with that
-- key in lock_keys, which changes it (see blocksSQL).
ALTER TABLE jobs ADD COLUMN lock_reads text[] NOT NULL DEFAULT '{}';
CREATE INDEX jobs_lock_reads ON jobs USING gin (lock_reads);
