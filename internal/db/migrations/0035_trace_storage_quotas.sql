-- NULL values inherit deployment defaults (global row) or the global quota
-- (per-account row). Changing quotas never changes consent or deletes content.
CREATE TABLE trace_quota_defaults (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    max_bytes bigint CHECK (max_bytes BETWEEN 1048576 AND 1099511627776 AND max_bytes % 1048576 = 0),
    max_traces bigint CHECK (max_traces BETWEEN 1 AND 1000000),
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    updated_at timestamptz,
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL
);
INSERT INTO trace_quota_defaults(singleton) VALUES (true);

CREATE TABLE trace_quota_overrides (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_bytes bigint CHECK (max_bytes BETWEEN 1048576 AND 1099511627776 AND max_bytes % 1048576 = 0),
    max_traces bigint CHECK (max_traces BETWEEN 1 AND 1000000),
    version bigint NOT NULL CHECK (version > 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL
);
