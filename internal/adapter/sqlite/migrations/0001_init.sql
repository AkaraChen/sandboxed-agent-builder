-- Builds, their attempts, attempt logs and retry idempotency keys.
-- Times are Unix nanoseconds (UTC).

CREATE TABLE builds (
    id                 TEXT PRIMARY KEY,
    caller             TEXT NOT NULL,
    idempotency_key    TEXT NOT NULL,
    request_hash       TEXT NOT NULL,
    spec_digest        TEXT NOT NULL,
    raw_spec           BLOB NOT NULL,
    resolved_spec      BLOB NOT NULL,
    no_cache           INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    current_attempt_id TEXT NOT NULL,
    attempts           INTEGER NOT NULL,
    UNIQUE (caller, idempotency_key)
);

CREATE TABLE attempts (
    id                  TEXT PRIMARY KEY,
    build_id            TEXT NOT NULL REFERENCES builds (id),
    number              INTEGER NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'interrupted')),
    created_at          INTEGER NOT NULL,
    started_at          INTEGER,
    ended_at            INTEGER,
    worker_id           TEXT,
    generation          INTEGER NOT NULL DEFAULT 0,
    lease_expires_at    INTEGER,
    cancel_requested_at INTEGER,
    failure_stage       TEXT,
    failure_code        TEXT,
    failure_message     TEXT,
    artifact            TEXT,
    recovered           INTEGER NOT NULL DEFAULT 0,
    log_seq             INTEGER NOT NULL DEFAULT 0,
    log_bytes           INTEGER NOT NULL DEFAULT 0,
    logs_truncated      INTEGER NOT NULL DEFAULT 0,
    UNIQUE (build_id, number)
);

CREATE INDEX attempts_queued ON attempts (created_at, id) WHERE status = 'queued';
CREATE INDEX attempts_running ON attempts (lease_expires_at) WHERE status = 'running';

CREATE TABLE attempt_logs (
    attempt_id TEXT NOT NULL REFERENCES attempts (id),
    seq        INTEGER NOT NULL,
    time       INTEGER NOT NULL,
    stage      TEXT NOT NULL,
    stream     TEXT NOT NULL,
    text       TEXT NOT NULL,
    PRIMARY KEY (attempt_id, seq)
) WITHOUT ROWID;

CREATE TABLE retries (
    caller          TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    build_id        TEXT NOT NULL REFERENCES builds (id),
    attempt_id      TEXT NOT NULL REFERENCES attempts (id),
    PRIMARY KEY (caller, idempotency_key)
);
