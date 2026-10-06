-- The status grid asks, for every team VM, how the last finished job that
-- touched it went; this index finds a VM's items without reading them all.
CREATE INDEX job_items_by_name ON job_items (name, job_id);
