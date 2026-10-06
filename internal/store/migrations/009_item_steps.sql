-- How each of an item's steps last ended (done, skipped, failed or
-- interrupted), by step, as its events arrive: the job page draws an item's
-- progress from it without reading the log.
ALTER TABLE job_items ADD COLUMN steps jsonb NOT NULL DEFAULT '{}';
