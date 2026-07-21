ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'tasks_version_positive'
          AND conrelid = 'tasks'::regclass
    ) THEN
        ALTER TABLE tasks
            ADD CONSTRAINT tasks_version_positive CHECK (version > 0);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS outbox_events (
    sequence BIGSERIAL PRIMARY KEY,
    event_id UUID NOT NULL UNIQUE,
    project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    aggregate_id BIGINT NOT NULL,
    aggregate_version BIGINT NOT NULL CHECK (aggregate_version > 0),
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_by TEXT,
    claim_until TIMESTAMPTZ,
    last_error TEXT,
    dead_at TIMESTAMPTZ
);

ALTER TABLE outbox_events
    ADD COLUMN IF NOT EXISTS dead_at TIMESTAMPTZ;

DROP INDEX IF EXISTS outbox_events_pending_idx;
CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at, sequence)
    WHERE published_at IS NULL AND dead_at IS NULL;
