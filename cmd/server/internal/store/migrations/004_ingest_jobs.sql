CREATE TABLE ingest_jobs (
    id text PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('sync', 'reembed')),
    source_id text REFERENCES sources(id) ON DELETE CASCADE,
    state text NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed')),
    progress jsonb NOT NULL DEFAULT '{}',
    result jsonb,
    error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz
);
CREATE UNIQUE INDEX ingest_jobs_active_sync ON ingest_jobs(source_id)
    WHERE kind = 'sync' AND state IN ('queued', 'running');
CREATE UNIQUE INDEX ingest_jobs_active_reembed ON ingest_jobs(kind)
    WHERE kind = 'reembed' AND state IN ('queued', 'running');
CREATE INDEX ingest_jobs_fifo ON ingest_jobs(created_at, id) WHERE state = 'queued';
CREATE INDEX ingest_jobs_source ON ingest_jobs(source_id, created_at DESC);
