-- How an interrupted item left its VM's config, for drift: 'untouched',
-- 'converged', or '' (may have changed); see store.ItemOutcome.LeftConfig.
ALTER TABLE job_items ADD COLUMN left_config text NOT NULL DEFAULT '';
