-- Only backend credential references are stored here, never resolved secrets.
CREATE TABLE endpoint_revisions (
    id bigserial PRIMARY KEY,
    model_name text NOT NULL,
    configuration jsonb,
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX endpoint_revisions_model ON endpoint_revisions(model_name, id DESC);
CREATE TABLE endpoint_active (
    model_name text PRIMARY KEY,
    revision_id bigint NOT NULL REFERENCES endpoint_revisions(id)
);
