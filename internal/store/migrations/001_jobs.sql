-- Jobs: one row per confirmed deploy, teardown, reset or power request.
CREATE TABLE jobs (
    id               bigserial PRIMARY KEY,
    kind             text        NOT NULL,
    inputs           jsonb       NOT NULL,          -- what the user asked for; re-planned at run time
    plan             jsonb       NOT NULL,          -- the preview the user confirmed, for display
    fingerprint      text        NOT NULL,          -- must still match the plan at run time
    lock_keys        text[]      NOT NULL,          -- team:NN, template:<name>, vmid:<n>; overlapping jobs wait
    status           text        NOT NULL DEFAULT 'pending',
    created_by       text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    finished_at      timestamptz,
    claimed_by       text,
    heartbeat_at     timestamptz,
    cancel_requested boolean     NOT NULL DEFAULT false,
    cancelled_by     text,
    summary          jsonb,
    error            text        NOT NULL DEFAULT ''
);
CREATE INDEX jobs_status_id ON jobs (status, id);
CREATE INDEX jobs_lock_keys ON jobs USING gin (lock_keys);

-- One row per VM in the plan, in plan order.
CREATE TABLE job_items (
    job_id     bigint      NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    idx        int         NOT NULL,
    name       text        NOT NULL,
    team       text        NOT NULL,
    vmid       int         NOT NULL,
    step       text        NOT NULL DEFAULT '',
    status     text        NOT NULL DEFAULT 'pending',
    error      text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, idx)
);
CREATE INDEX job_items_name ON job_items (job_id, name);

-- Progress log, for the job page and history.
CREATE TABLE job_events (
    id      bigserial PRIMARY KEY,
    job_id  bigint      NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    at      timestamptz NOT NULL,
    item    text        NOT NULL,
    step    text        NOT NULL,
    status  text        NOT NULL,
    message text        NOT NULL
);
CREATE INDEX job_events_job ON job_events (job_id, id);
