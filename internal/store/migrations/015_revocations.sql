-- Refresh tokens to revoke at the identity provider, queued in the same
-- statement that ends their session, so a crash can't lose one. A worker
-- leases a row (next_at) while it tries, and deletes it once done.
CREATE TABLE revocations (
    id            bigserial   PRIMARY KEY,
    session_id    text        NOT NULL,          -- what the token was sealed for
    refresh_token text        NOT NULL,          -- sealed, as sessions.refresh_token
    attempts      int         NOT NULL DEFAULT 0,
    next_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX revocations_next_at ON revocations (next_at);
