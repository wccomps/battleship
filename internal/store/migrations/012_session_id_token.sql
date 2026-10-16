-- The session's latest ID token, sealed, sent as id_token_hint on logout.
ALTER TABLE sessions ADD COLUMN id_token_sealed text NOT NULL DEFAULT '';
