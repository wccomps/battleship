-- The role a job's creator had when they submitted it, for the audit
-- trail; empty for jobs from the command line, which has no roles.
ALTER TABLE jobs ADD COLUMN created_role text NOT NULL DEFAULT '';

-- Previews shown in the web app. A confirm must name one of its own
-- session's previews, submit the same inputs, and still match its
-- fingerprint; submitting sets job_id, so a double-clicked confirm makes
-- one job. Previews go with their session.
CREATE TABLE previews (
    id          text        PRIMARY KEY,    -- SHA-256 hex of the nonce in the confirm form
    session_id  text        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kind        text        NOT NULL,
    inputs      jsonb       NOT NULL,       -- what was previewed, as jobs.Inputs
    fingerprint text        NOT NULL,       -- of the plan the user saw
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL,
    job_id      bigint      REFERENCES jobs (id) ON DELETE CASCADE   -- set once submitted
);
CREATE INDEX previews_session ON previews (session_id, expires_at);
