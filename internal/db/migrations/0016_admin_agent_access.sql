-- Administration credentials are deliberately isolated from inference keys.
CREATE TABLE admin_api_keys (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 owner_id uuid NOT NULL REFERENCES users(id),
 parent_id uuid REFERENCES admin_api_keys(id),
 label text NOT NULL CHECK(length(label) BETWEEN 1 AND 100),
 prefix text NOT NULL,
 hash bytea NOT NULL UNIQUE CHECK(octet_length(hash)=32),
 permissions text[] NOT NULL CHECK(cardinality(permissions)>0),
 allowed_networks text[] NOT NULL CHECK(cardinality(allowed_networks)>0),
 created timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires timestamptz NOT NULL,
 revoked boolean NOT NULL DEFAULT false,
 policy_version bigint NOT NULL DEFAULT 1,
 last_used timestamptz,
 last_ip text
);
CREATE INDEX admin_api_keys_owner ON admin_api_keys(owner_id);
CREATE INDEX admin_api_keys_parent ON admin_api_keys(parent_id);
CREATE TABLE admin_changes (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 key_id uuid NOT NULL REFERENCES admin_api_keys(id),
 operation text NOT NULL,
 arguments_hash bytea NOT NULL,
 state_version bigint NOT NULL,
 policy_version bigint NOT NULL,
 summary jsonb NOT NULL,
 created timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires timestamptz NOT NULL DEFAULT clock_timestamp()+interval '10 minutes',
 status text NOT NULL DEFAULT 'previewed' CHECK(status IN ('previewed','running','succeeded','failed')),
 idempotency_key text,
 response_status integer,
 completed timestamptz
);
CREATE UNIQUE INDEX admin_changes_idempotency ON admin_changes(key_id,idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE TABLE admin_state (id boolean PRIMARY KEY DEFAULT true CHECK(id), version bigint NOT NULL DEFAULT 1);
INSERT INTO admin_state(id) VALUES(true);
CREATE FUNCTION bump_admin_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE admin_state SET version=version+1;
 RETURN NULL;
END $$;
-- Previews become stale on relevant changes made through any interface, including CLI.
CREATE TRIGGER admin_users_changed AFTER INSERT OR UPDATE OF groups,enabled,protected_admin,username,email OR DELETE ON users FOR EACH STATEMENT EXECUTE FUNCTION bump_admin_state();
CREATE TRIGGER admin_overrides_changed AFTER INSERT OR UPDATE OR DELETE ON quota_overrides FOR EACH STATEMENT EXECUTE FUNCTION bump_admin_state();
CREATE TRIGGER admin_settings_changed AFTER INSERT OR UPDATE OR DELETE ON settings_state FOR EACH STATEMENT EXECUTE FUNCTION bump_admin_state();
CREATE TRIGGER admin_endpoints_changed AFTER INSERT OR UPDATE OR DELETE ON endpoint_active FOR EACH STATEMENT EXECUTE FUNCTION bump_admin_state();
CREATE TRIGGER admin_keys_changed AFTER INSERT OR UPDATE OF revoked,permissions,allowed_networks,expires OR DELETE ON admin_api_keys FOR EACH STATEMENT EXECUTE FUNCTION bump_admin_state();
CREATE FUNCTION invalidate_admin_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.groups IS DISTINCT FROM NEW.groups OR OLD.enabled IS DISTINCT FROM NEW.enabled OR OLD.protected_admin IS DISTINCT FROM NEW.protected_admin THEN
  UPDATE admin_api_keys SET revoked=true,policy_version=policy_version+1 WHERE owner_id=NEW.id AND NOT revoked;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER invalidate_admin_owner AFTER UPDATE ON users FOR EACH ROW EXECUTE FUNCTION invalidate_admin_owner();
CREATE FUNCTION invalidate_admin_password() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE admin_api_keys SET revoked=true,policy_version=policy_version+1 WHERE owner_id=NEW.user_id AND NOT revoked;
 RETURN NEW;
END $$;
CREATE TRIGGER invalidate_admin_password AFTER UPDATE ON local_credentials FOR EACH ROW WHEN (OLD.revision IS DISTINCT FROM NEW.revision) EXECUTE FUNCTION invalidate_admin_password();
CREATE TRIGGER admin_password_changed AFTER INSERT OR UPDATE OR DELETE ON local_credentials FOR EACH STATEMENT EXECUTE FUNCTION bump_admin_state();
CREATE TRIGGER admin_inference_keys_changed AFTER INSERT OR UPDATE OF revoked OR DELETE ON api_keys FOR EACH STATEMENT EXECUTE FUNCTION bump_admin_state();
-- Managed routing demotions invalidate keys in the activation transaction,
-- including changes made by the operator CLI while a router is offline.
CREATE FUNCTION invalidate_admin_routing() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE active_document jsonb;
BEGIN
 IF NEW.domain='routing' AND (TG_OP='INSERT' OR OLD.active_revision IS DISTINCT FROM NEW.active_revision) THEN
  SELECT document INTO active_document FROM settings_revisions WHERE id=NEW.active_revision;
  UPDATE admin_api_keys k SET revoked=true,policy_version=policy_version+1
  FROM users u WHERE k.owner_id=u.id AND NOT k.revoked AND NOT u.protected_admin
  AND NOT EXISTS (SELECT 1 FROM unnest(u.groups) AS membership(name)
   WHERE COALESCE((active_document->'groups'->membership.name->>'admin')::boolean,false));
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER invalidate_admin_routing AFTER INSERT OR UPDATE OF active_revision ON settings_state FOR EACH ROW EXECUTE FUNCTION invalidate_admin_routing();
