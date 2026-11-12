-- Managed settings: immutable revisions per settings domain, one state row
-- naming the active revision and the pending draft, encrypted secret values
-- and append-only administration audit events. Revision documents and audit
-- rows hold secret references and field names only, never secret plaintext.
-- Actor columns are plain UUIDs without foreign keys, so history survives the
-- erasure of the acting user; pseudonymization rules belong to the audit design.

CREATE FUNCTION forbid_history_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are append-only', TG_TABLE_NAME
        USING ERRCODE = '23514', CONSTRAINT = 'append_only_history';
END;
$$;

-- Ciphertext is AES-256-GCM under a key supplied outside the database; the
-- additional data binds it to its own id and name.
CREATE TABLE managed_secrets (
    id uuid PRIMARY KEY,
    name text NOT NULL CHECK (name ~ '^[a-z_]+\.[a-z_]+$'),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) > 28),
    key_id text NOT NULL CHECK (key_id ~ '^[0-9a-f]{24}$'),
    created_by uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER managed_secrets_append_only BEFORE UPDATE OR DELETE ON managed_secrets
    FOR EACH ROW EXECUTE FUNCTION forbid_history_change();
CREATE TRIGGER managed_secrets_no_truncate BEFORE TRUNCATE ON managed_secrets
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_history_change();

CREATE TABLE settings_revisions (
    id bigserial PRIMARY KEY,
    domain text NOT NULL CHECK (domain IN ('authentication')),
    origin text NOT NULL CHECK (origin IN ('import', 'draft')),
    document jsonb NOT NULL CHECK (jsonb_typeof(document) = 'object'),
    based_on bigint REFERENCES settings_revisions(id),
    created_by uuid,
    created_by_label text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- The latest check result; problems are field-level and redacted. This is
    -- the only column that may change after insertion.
    validation jsonb
);
CREATE INDEX settings_revisions_domain ON settings_revisions(domain, id DESC);
CREATE FUNCTION guard_settings_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' OR NEW.id <> OLD.id OR NEW.domain <> OLD.domain OR NEW.origin <> OLD.origin
        OR NEW.document <> OLD.document OR NEW.based_on IS DISTINCT FROM OLD.based_on
        OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_by_label <> OLD.created_by_label
        OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'settings revisions are immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'append_only_history';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER settings_revisions_immutable BEFORE UPDATE OR DELETE ON settings_revisions
    FOR EACH ROW EXECUTE FUNCTION guard_settings_revision();
CREATE TRIGGER settings_revisions_no_truncate BEFORE TRUNCATE ON settings_revisions
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_history_change();

-- A domain without a row is file-managed. version increases with every draft
-- save, discard, activation and rollback (optimistic concurrency).
CREATE TABLE settings_state (
    domain text PRIMARY KEY CHECK (domain IN ('authentication')),
    active_revision bigint NOT NULL REFERENCES settings_revisions(id),
    draft_revision bigint REFERENCES settings_revisions(id),
    version bigint NOT NULL CHECK (version > 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (draft_revision IS DISTINCT FROM active_revision)
);

CREATE TABLE settings_activations (
    id bigserial PRIMARY KEY,
    domain text NOT NULL,
    revision_id bigint NOT NULL REFERENCES settings_revisions(id),
    kind text NOT NULL CHECK (kind IN ('import', 'activate', 'rollback')),
    activated_by uuid,
    activated_by_label text NOT NULL,
    activated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX settings_activations_domain ON settings_activations(domain, id DESC);
CREATE TRIGGER settings_activations_append_only BEFORE UPDATE OR DELETE ON settings_activations
    FOR EACH ROW EXECUTE FUNCTION forbid_history_change();
CREATE TRIGGER settings_activations_no_truncate BEFORE TRUNCATE ON settings_activations
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_history_change();

CREATE TABLE audit_events (
    id bigserial PRIMARY KEY,
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    action text NOT NULL CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'),
    actor_type text NOT NULL,
    actor_id uuid,
    actor_name text,
    actor_host text,
    target_type text,
    target_id text,
    revision bigint,
    result text NOT NULL CHECK (result IN ('succeeded', 'refused', 'failed')),
    reason text,
    details jsonb CHECK (details IS NULL OR jsonb_typeof(details) = 'object')
);
CREATE INDEX audit_events_occurred ON audit_events(occurred_at DESC);
CREATE INDEX audit_events_target ON audit_events(target_type, target_id, id DESC);
CREATE TRIGGER audit_events_append_only BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION forbid_history_change();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_history_change();
