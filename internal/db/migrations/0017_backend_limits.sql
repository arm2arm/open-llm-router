-- Backend budgets are shared by router processes, independently of user quotas.
CREATE TABLE backend_rate_state (
    backend text PRIMARY KEY,
    window_start timestamptz NOT NULL,
    requests bigint NOT NULL DEFAULT 0 CHECK (requests >= 0),
    tokens bigint NOT NULL DEFAULT 0 CHECK (tokens >= 0)
);
CREATE TABLE backend_rate_leases (
    id text PRIMARY KEY,
    backend text NOT NULL REFERENCES backend_rate_state(backend) ON DELETE CASCADE,
    reserved_tokens bigint NOT NULL CHECK (reserved_tokens >= 0),
    expires_at timestamptz NOT NULL
);
CREATE INDEX backend_rate_leases_backend ON backend_rate_leases(backend, expires_at);
