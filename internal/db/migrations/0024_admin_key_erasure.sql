-- Erasure includes active, revoked and expired administration credentials and
-- all delegated descendants/previews. Audit events remain append-only.
ALTER TABLE admin_api_keys DROP CONSTRAINT admin_api_keys_owner_id_fkey;
ALTER TABLE admin_api_keys ADD CONSTRAINT admin_api_keys_owner_id_fkey FOREIGN KEY(owner_id) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE admin_api_keys DROP CONSTRAINT admin_api_keys_parent_id_fkey;
ALTER TABLE admin_api_keys ADD CONSTRAINT admin_api_keys_parent_id_fkey FOREIGN KEY(parent_id) REFERENCES admin_api_keys(id) ON DELETE CASCADE;
ALTER TABLE admin_changes DROP CONSTRAINT admin_changes_key_id_fkey;
ALTER TABLE admin_changes ADD CONSTRAINT admin_changes_key_id_fkey FOREIGN KEY(key_id) REFERENCES admin_api_keys(id) ON DELETE CASCADE;
