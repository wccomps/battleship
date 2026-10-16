-- Lock keys starting "read:" are shared: two jobs that only read a VM (say,
-- clone the same base image) run together, and wait only for a job that
-- holds the same key without the prefix (one that changes the VM).
CREATE FUNCTION lock_writes(keys text[]) RETURNS text[] LANGUAGE sql IMMUTABLE AS
    $$ SELECT coalesce(array_agg(k), '{}') FROM unnest(keys) k WHERE k NOT LIKE 'read:%' $$;
CREATE FUNCTION lock_reads(keys text[]) RETURNS text[] LANGUAGE sql IMMUTABLE AS
    $$ SELECT coalesce(array_agg(substr(k, 6)), '{}') FROM unnest(keys) k WHERE k LIKE 'read:%' $$;
ALTER TABLE jobs
    ADD COLUMN lock_writes text[] GENERATED ALWAYS AS (lock_writes(lock_keys)) STORED,
    ADD COLUMN lock_reads  text[] GENERATED ALWAYS AS (lock_reads(lock_keys)) STORED;
CREATE INDEX jobs_lock_writes ON jobs USING gin (lock_writes);
CREATE INDEX jobs_lock_reads ON jobs USING gin (lock_reads);
