-- The replica that must run this job; NULL lets any replica run it.
ALTER TABLE jobs ADD COLUMN replica text;
