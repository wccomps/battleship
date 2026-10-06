-- Web sessions: one row per logged-in browser.
CREATE TABLE sessions (
    id            text        PRIMARY KEY,   -- SHA-256 hex of the cookie token; never the token itself
    subject       text        NOT NULL,      -- the identity provider's user ID
    name          text        NOT NULL,
    email         text        NOT NULL,
    groups        text[]      NOT NULL,      -- identity-provider groups as of the last login or refresh
    refresh_token text        NOT NULL,      -- used every web.session_refresh to re-read the groups
    csrf          text        NOT NULL,      -- every state-changing request must carry it
    created_at    timestamptz NOT NULL,      -- login time; the absolute expiry counts from it
    last_seen     timestamptz NOT NULL,      -- last request; the idle expiry counts from it
    refreshed_at  timestamptz NOT NULL,      -- last successful group check
    expires_at    timestamptz NOT NULL,      -- earliest of the idle and absolute expiry
    refresh_retry_at  timestamptz,           -- no new refresh before this: one is running, or backing off after a failure
    refresh_failed_at timestamptz            -- last failure to reach the identity provider since the last success
);
CREATE INDEX sessions_expires_at ON sessions (expires_at);
