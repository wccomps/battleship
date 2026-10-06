-- Items that never reached a step get a status of their own, "not run",
-- instead of staying pending or reading as interrupted in finished jobs.
UPDATE job_items i SET status = 'not run'
FROM jobs j
WHERE j.id = i.job_id AND j.finished_at IS NOT NULL
  AND i.step = '' AND i.status IN ('pending', 'running', 'interrupted');
