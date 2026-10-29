-- kind stays for one release so older replicas still work; with it always
-- 'ticket', job_credentials_check keeps renew_after non-NULL.
DELETE FROM job_credentials WHERE kind <> 'ticket';
ALTER TABLE job_credentials ALTER COLUMN kind SET DEFAULT 'ticket';
