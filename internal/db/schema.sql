-- Pilot schema. Applied in one transaction on every startup; introduce versioned
-- migrations when this schema first changes after the pilot.
CREATE TABLE IF NOT EXISTS users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    issuer text NOT NULL,
    sub text NOT NULL,
    username text NOT NULL DEFAULT '',
    email text NOT NULL DEFAULT '',
    groups text[] NOT NULL DEFAULT '{}',
    enabled boolean NOT NULL DEFAULT true,
    synced_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (issuer, sub)
);

CREATE TABLE IF NOT EXISTS api_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    prefix text NOT NULL,
    hash bytea NOT NULL UNIQUE,
    label text NOT NULL DEFAULT '',
    class text NOT NULL DEFAULT 'interactive',
    trusted_forwarder boolean NOT NULL DEFAULT false,
    created timestamptz NOT NULL DEFAULT now(),
    last_used timestamptz,
    expires timestamptz,
    revoked boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS api_keys_user_id_idx ON api_keys (user_id);

CREATE TABLE IF NOT EXISTS sessions (
    id_hash bytea PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created timestamptz NOT NULL DEFAULT now(),
    expires timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id);

CREATE TABLE IF NOT EXISTS usage (
    ts timestamptz NOT NULL DEFAULT now(),
    key_id uuid REFERENCES api_keys(id) ON DELETE SET NULL,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    forwarded_for uuid REFERENCES users(id) ON DELETE CASCADE,
    model text NOT NULL,
    replica text NOT NULL,
    protocol text NOT NULL,
    -- HTTP status as text, 'aborted' for incomplete responses, or 'error' for an SSE error.
    status text NOT NULL,
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    latency_ms bigint NOT NULL,
    ttfb_ms bigint
);
CREATE INDEX IF NOT EXISTS usage_user_id_ts_idx ON usage (user_id, ts);
CREATE INDEX IF NOT EXISTS usage_key_id_idx ON usage (key_id);
CREATE INDEX IF NOT EXISTS usage_forwarded_for_idx ON usage (forwarded_for);

CREATE TABLE IF NOT EXISTS usage_daily (
    day date NOT NULL,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    model text NOT NULL,
    requests bigint NOT NULL DEFAULT 0,
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (day, user_id, model)
);
