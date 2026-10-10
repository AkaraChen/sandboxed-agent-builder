-- Builds, their attempts, attempt logs and retry idempotency keys.

-- +goose Up
CREATE TABLE builds (
    id                 text PRIMARY KEY,
    caller             text NOT NULL,
    idempotency_key    text NOT NULL,
    request_hash       text NOT NULL,
    spec_digest        text NOT NULL,
    -- The request body exactly as received.
    raw_spec           bytea NOT NULL,
    resolved_spec      jsonb NOT NULL,
    no_cache           boolean NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL,
    current_attempt_id text NOT NULL,
    attempts           integer NOT NULL,
    UNIQUE (caller, idempotency_key)
);

CREATE TABLE attempts (
    id                  text PRIMARY KEY,
    build_id            text NOT NULL REFERENCES builds (id),
    number              integer NOT NULL,
    status              text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'interrupted')),
    created_at          timestamptz NOT NULL,
    started_at          timestamptz,
    ended_at            timestamptz,
    worker_id           text,
    generation          bigint NOT NULL DEFAULT 0,
    lease_expires_at    timestamptz,
    cancel_requested_at timestamptz,
    failure             jsonb,
    artifact            jsonb,
    recovered           boolean NOT NULL DEFAULT false,
    log_seq             bigint NOT NULL DEFAULT 0,
    log_bytes           bigint NOT NULL DEFAULT 0,
    logs_truncated      boolean NOT NULL DEFAULT false,
    UNIQUE (build_id, number)
);

CREATE INDEX attempts_queued ON attempts (created_at, id) WHERE status = 'queued';
CREATE INDEX attempts_running ON attempts (lease_expires_at) WHERE status = 'running';

CREATE TABLE attempt_logs (
    attempt_id text NOT NULL REFERENCES attempts (id),
    seq        bigint NOT NULL,
    time       timestamptz NOT NULL,
    stage      text NOT NULL,
    stream     text NOT NULL,
    text       text NOT NULL,
    PRIMARY KEY (attempt_id, seq)
);

CREATE TABLE retries (
    caller          text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash    text NOT NULL,
    build_id        text NOT NULL REFERENCES builds (id),
    attempt_id      text NOT NULL REFERENCES attempts (id),
    PRIMARY KEY (caller, idempotency_key)
);

-- +goose Down
DROP TABLE retries, attempt_logs, attempts, builds;
