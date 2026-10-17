-- An item's last info line since its step began or ended, e.g. what a long
-- step waits for: the job page shows it beside the step without reading the
-- log.
ALTER TABLE job_items ADD COLUMN note text NOT NULL DEFAULT '';
