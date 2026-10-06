-- Battleship holds no Proxmox credential of its own: every call is made as a
-- person. A session keeps its user's Proxmox ticket, and a job the
-- credential of whoever submitted it.

-- The session's Proxmox ticket and CSRF token, each sealed for the session
-- (see auth's ticket.go), and a Proxmox login in progress.
ALTER TABLE sessions
    ADD COLUMN username          text NOT NULL DEFAULT '',  -- preferred_username: the Proxmox realm's user is <username>@<realm>
    ADD COLUMN pve_user          text NOT NULL DEFAULT '',  -- e.g. jdoe@auth.example.org
    ADD COLUMN pve_ticket_sealed text NOT NULL DEFAULT '',
    ADD COLUMN pve_csrf_sealed   text NOT NULL DEFAULT '',
    ADD COLUMN pve_issued_at     timestamptz,               -- when the ticket was issued or last renewed
    ADD COLUMN pve_login_at      timestamptz,               -- when the Proxmox login that started it ran; renewals keep it
    ADD COLUMN pve_login_state    text NOT NULL DEFAULT '', -- SHA-256 hex of the state of a Proxmox login in progress
    ADD COLUMN pve_login_next     text NOT NULL DEFAULT '', -- where that login goes afterwards
    ADD COLUMN pve_login_endpoint text NOT NULL DEFAULT ''; -- the node that started it: it alone can finish it

-- Who a job acts as in Proxmox: its submitter's Proxmox user, or their API
-- token's ID. Battleship has no roles any more; created_role stays for the
-- jobs that recorded one.
ALTER TABLE jobs ADD COLUMN created_as text NOT NULL DEFAULT '';

-- A job's credential: the submitter's ticket (renewed while the job waits
-- and runs) or their own API token, sealed for the job. It goes when the
-- job ends.
CREATE TABLE job_credentials (
    job_id      bigint      PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
    kind        text        NOT NULL CHECK (kind IN ('ticket', 'token')),
    pve_user    text        NOT NULL,
    sealed      text        NOT NULL,
    issued_at   timestamptz NOT NULL,
    login_at    timestamptz NOT NULL,   -- the submitter's Proxmox login; past proxmox.ticket_max_age the ticket lapses
    renew_after timestamptz,            -- tickets: when to renew; NULL for tokens
    CHECK ((kind = 'ticket') = (renew_after IS NOT NULL))
);
CREATE INDEX job_credentials_renew_after ON job_credentials (renew_after) WHERE renew_after IS NOT NULL;

-- Whatever ends a job (finish, cancel, reap, stale) deletes its
-- credential, so a finished job can't be used to act as its submitter.
CREATE FUNCTION job_credentials_end() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM job_credentials WHERE job_id = NEW.id;
    RETURN NEW;
END $$;
CREATE TRIGGER jobs_end_credentials AFTER UPDATE OF status ON jobs
    FOR EACH ROW WHEN (NEW.status NOT IN ('pending', 'running'))
    EXECUTE FUNCTION job_credentials_end();

-- A value sealed with database.seal_key the first time a process used the
-- database, so a process with another key refuses to start rather than
-- take every job credential it can't open for lapsed.
CREATE TABLE seal_check (
    id     int  PRIMARY KEY CHECK (id = 1),
    sealed text NOT NULL
);
