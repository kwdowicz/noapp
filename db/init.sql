CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL CHECK (length(trim(name)) > 0),
    email TEXT NOT NULL UNIQUE CHECK (length(trim(email)) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS projects (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL CHECK (length(trim(name)) > 0),
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tasks (
    id BIGSERIAL PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    assignee_id BIGINT REFERENCES users(id) ON DELETE SET NULL,
    title TEXT NOT NULL CHECK (length(trim(title)) > 0),
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'todo' CHECK (status IN ('todo', 'in_progress', 'done')),
    version BIGINT NOT NULL DEFAULT 1 CONSTRAINT tasks_version_positive CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tasks_project_id_idx ON tasks(project_id);
CREATE INDEX IF NOT EXISTS tasks_assignee_id_idx ON tasks(assignee_id);

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

CREATE INDEX IF NOT EXISTS outbox_events_pending_idx
    ON outbox_events (next_attempt_at, sequence)
    WHERE published_at IS NULL AND dead_at IS NULL;

INSERT INTO users (name, email) VALUES
    ('Ada Lovelace', 'ada@example.test'),
    ('Grace Hopper', 'grace@example.test')
ON CONFLICT (email) DO NOTHING;

INSERT INTO projects (name, description)
SELECT 'Observability Lab', 'A sample project ready for experimentation.'
WHERE NOT EXISTS (SELECT 1 FROM projects);

INSERT INTO tasks (project_id, assignee_id, title, description, status)
SELECT p.id, u.id, 'Build the sample application', 'Create a useful baseline before adding instrumentation.', 'in_progress'
FROM projects p CROSS JOIN users u
WHERE p.name = 'Observability Lab' AND u.email = 'ada@example.test'
  AND NOT EXISTS (SELECT 1 FROM tasks)
LIMIT 1;
