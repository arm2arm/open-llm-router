-- Local passwords are separate from OIDC identities. Only the bootstrap path
-- creates the singleton protected administrator; no migration ships a password.
ALTER TABLE users ADD COLUMN protected_admin boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD CONSTRAINT protected_admin_identity CHECK (
    NOT protected_admin OR (enabled AND issuer = 'local' AND sub = 'admin' AND username = 'admin')
);
CREATE UNIQUE INDEX protected_admin_singleton ON users (protected_admin) WHERE protected_admin;

CREATE TABLE local_credentials (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    version smallint NOT NULL DEFAULT 1 CHECK (version = 1),
    -- Version 1: Argon2id v19, 64 MiB, 3 passes, 2 lanes, 16-byte salt,
    -- 32-byte output, unpadded base64. Bound the format before verification.
    password_hash text NOT NULL CHECK (password_hash ~
        '^\$argon2id\$v=19\$m=65536,t=3,p=2\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$'),
    must_change_password boolean NOT NULL DEFAULT true,
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- A protected account cannot exist at COMMIT without its local credential.
-- Deferral permits bootstrap to insert the user and credential atomically.
ALTER TABLE users ADD COLUMN protected_credential_user_id uuid GENERATED ALWAYS AS
    (CASE WHEN protected_admin THEN id ELSE NULL END) STORED;
ALTER TABLE users ADD CONSTRAINT protected_admin_credentials_required
    FOREIGN KEY (protected_credential_user_id) REFERENCES local_credentials(user_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION guard_protected_admin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        IF EXISTS (SELECT 1 FROM users WHERE protected_admin) THEN
            RAISE EXCEPTION 'protected local administrator cannot be removed'
                USING ERRCODE = '23514', CONSTRAINT = 'protected_admin_guard';
        END IF;
        RETURN NULL;
    END IF;
    IF OLD.protected_admin THEN
        IF TG_OP = 'DELETE' THEN
            RAISE EXCEPTION 'protected local administrator cannot be removed'
                USING ERRCODE = '23514', CONSTRAINT = 'protected_admin_guard';
        END IF;
        IF NOT NEW.protected_admin OR NOT NEW.enabled OR NEW.id <> OLD.id
            OR NEW.issuer <> OLD.issuer OR NEW.sub <> OLD.sub OR NEW.username <> OLD.username THEN
            RAISE EXCEPTION 'protected local administrator cannot be disabled or demoted'
                USING ERRCODE = '23514', CONSTRAINT = 'protected_admin_guard';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    -- Local credentials cannot be attached to an SSO identity.
    IF NEW.issuer <> 'local' AND EXISTS (SELECT 1 FROM local_credentials WHERE user_id = OLD.id) THEN
        RAISE EXCEPTION 'local credential requires a local identity'
            USING ERRCODE = '23514', CONSTRAINT = 'local_credentials_identity';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER protected_admin_lifecycle BEFORE UPDATE OR DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION guard_protected_admin();
CREATE TRIGGER protected_admin_truncate BEFORE TRUNCATE ON users
    FOR EACH STATEMENT EXECUTE FUNCTION guard_protected_admin();

CREATE FUNCTION guard_local_credential() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        IF EXISTS (SELECT 1 FROM users WHERE protected_admin) THEN
            RAISE EXCEPTION 'protected local administrator credential cannot be removed'
                USING ERRCODE = '23514', CONSTRAINT = 'protected_admin_guard';
        END IF;
        RETURN NULL;
    END IF;
    IF TG_OP <> 'INSERT' AND EXISTS (SELECT 1 FROM users WHERE id = OLD.user_id AND protected_admin) THEN
        IF TG_OP = 'DELETE' THEN
            RAISE EXCEPTION 'protected local administrator credential cannot be removed'
                USING ERRCODE = '23514', CONSTRAINT = 'protected_admin_guard';
        END IF;
        IF NEW.user_id <> OLD.user_id THEN
            RAISE EXCEPTION 'protected local administrator credential cannot be detached'
                USING ERRCODE = '23514', CONSTRAINT = 'protected_admin_guard';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND issuer = 'local') THEN
        RAISE EXCEPTION 'local credential requires a local identity'
            USING ERRCODE = '23514', CONSTRAINT = 'local_credentials_identity';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER local_credential_lifecycle BEFORE INSERT OR UPDATE OR DELETE ON local_credentials
    FOR EACH ROW EXECUTE FUNCTION guard_local_credential();
CREATE TRIGGER local_credential_truncate BEFORE TRUNCATE ON local_credentials
    FOR EACH STATEMENT EXECUTE FUNCTION guard_local_credential();
