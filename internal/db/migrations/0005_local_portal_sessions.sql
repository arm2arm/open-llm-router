-- Bind local sessions to the exact credential revision verified at login.
-- Existing sessions came from OIDC and retain their original lifetime.
ALTER TABLE local_credentials ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0);
CREATE FUNCTION advance_local_credential_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.must_change_password AND NOT NEW.must_change_password AND NEW.password_hash=OLD.password_hash THEN
        RAISE EXCEPTION 'initial password change requires a replacement credential'
            USING ERRCODE='23514', CONSTRAINT='local_initial_password_change';
    END IF;
    IF NEW.password_hash<>OLD.password_hash OR NEW.version<>OLD.version OR NEW.must_change_password<>OLD.must_change_password THEN
        NEW.revision := OLD.revision+1;
        NEW.changed_at := clock_timestamp();
    ELSIF NEW.revision<>OLD.revision THEN
        RAISE EXCEPTION 'credential revision is managed by password changes'
            USING ERRCODE='23514', CONSTRAINT='local_credential_revision';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER local_credential_revision BEFORE UPDATE ON local_credentials
    FOR EACH ROW EXECUTE FUNCTION advance_local_credential_revision();
ALTER TABLE sessions ADD COLUMN auth_method text NOT NULL DEFAULT 'oidc' CHECK (auth_method IN ('oidc','local'));
ALTER TABLE sessions ADD COLUMN scope text NOT NULL DEFAULT 'full' CHECK (scope IN ('full','password_change'));
ALTER TABLE sessions ADD COLUMN credential_revision bigint;
ALTER TABLE sessions ADD CONSTRAINT session_auth_scope CHECK (
    (auth_method='oidc' AND scope='full' AND credential_revision IS NULL) OR
    (auth_method='local' AND credential_revision > 0 AND credential_revision IS NOT NULL)
);

-- Fixed-window login limits survive restarts and are shared by router instances.
-- Buckets are SHA-256 digests, never raw usernames or client addresses.
CREATE TABLE portal_auth_throttle (
    bucket bytea PRIMARY KEY CHECK (octet_length(bucket)=32),
    attempts integer NOT NULL CHECK (attempts > 0),
    expires timestamptz NOT NULL
);
CREATE INDEX portal_auth_throttle_expiry ON portal_auth_throttle(expires);
