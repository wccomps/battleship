-- The seal labels for job credentials and the key check were renamed, so
-- what was sealed under the old ones can't be opened. Drop it: the key check
-- is sealed again at startup, and only pending or running jobs had
-- credentials (they end as "authorization lapsed").
DELETE FROM job_credentials;
DELETE FROM seal_check;
