-- Per-user limits that replace the group policy for one field. A NULL value
-- means unlimited. Changes reach running routers through the API-key cache
-- invalidation, without a restart.
CREATE TABLE quota_overrides (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    field text NOT NULL CHECK (field IN ('tokens_per_day', 'tokens_per_month', 'rpm', 'tpm',
        'max_concurrent', 'max_concurrent_batch', 'strict_quota')),
    value bigint,
    reason text NOT NULL DEFAULT '',
    set_by text NOT NULL DEFAULT '',
    set_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, field)
);

ALTER TABLE api_keys ADD CONSTRAINT api_keys_class_check CHECK (class IN ('interactive', 'batch'));
