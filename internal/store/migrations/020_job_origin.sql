-- What submitted the job: '' the volunteer pages, 'session' the session
-- interface.
ALTER TABLE jobs ADD COLUMN origin text NOT NULL DEFAULT '';
