-- +goose Up
-- +goose StatementBegin

ALTER TABLE outbox_jobs DROP CONSTRAINT outbox_jobs_status_check;
ALTER TABLE outbox_jobs ADD CONSTRAINT outbox_jobs_status_check CHECK (
    status IN ('pending', 'running', 'succeeded', 'retryable_failed', 'dead_pending', 'dead')
);

DROP INDEX outbox_jobs_active_dedupe_key;
CREATE UNIQUE INDEX outbox_jobs_active_dedupe_key
    ON outbox_jobs (type, dedupe_key)
    WHERE status IN ('pending', 'running', 'retryable_failed', 'dead_pending');

DROP INDEX outbox_jobs_claim_idx;
CREATE INDEX outbox_jobs_claim_idx
    ON outbox_jobs (next_run_at)
    WHERE status IN ('pending', 'retryable_failed', 'running', 'dead_pending');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX outbox_jobs_claim_idx;
CREATE INDEX outbox_jobs_claim_idx
    ON outbox_jobs (next_run_at)
    WHERE status IN ('pending', 'retryable_failed', 'running');

DROP INDEX outbox_jobs_active_dedupe_key;
CREATE UNIQUE INDEX outbox_jobs_active_dedupe_key
    ON outbox_jobs (type, dedupe_key)
    WHERE status IN ('pending', 'running', 'retryable_failed');

ALTER TABLE outbox_jobs DROP CONSTRAINT outbox_jobs_status_check;
ALTER TABLE outbox_jobs ADD CONSTRAINT outbox_jobs_status_check CHECK (
    status IN ('pending', 'running', 'succeeded', 'retryable_failed', 'dead')
);

-- +goose StatementEnd
