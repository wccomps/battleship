-- Before 007, history skipped an item interrupted at a power step (stop,
-- start, power) as having left the config as the job before did. Carry that
-- forward for the rows written then: 007 gave them '' (may have changed).
UPDATE job_items SET left_config = 'untouched'
WHERE status = 'interrupted' AND left_config = '' AND step IN ('stop', 'start', 'power');
