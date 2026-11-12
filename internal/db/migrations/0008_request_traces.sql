-- Content is separate from quota accounting and has a shorter, personal lifetime.
ALTER TABLE users ADD COLUMN trace_enabled boolean NOT NULL DEFAULT true;
ALTER TABLE users ADD COLUMN trace_retention_days integer NOT NULL DEFAULT 7 CHECK (trace_retention_days BETWEEN 1 AND 30);
ALTER TABLE users ADD COLUMN trace_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE usage ADD COLUMN request_id text;
ALTER TABLE usage ADD COLUMN operation text NOT NULL DEFAULT '';
CREATE TABLE request_traces (
    request_id text PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    started_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    detail jsonb NOT NULL
);
CREATE INDEX request_traces_user_time_idx ON request_traces (user_id, started_at DESC);
CREATE INDEX request_traces_expiry_idx ON request_traces (expires_at);
