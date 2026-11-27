-- An inference key issued through the administration API (user_keys:issue)
-- names the administration key that issued it. Only that key may revoke or
-- summarize it through the API; the owner keeps full control in the portal.
-- Erasing the administration key's owner keeps the user's key unattributed.
ALTER TABLE api_keys ADD COLUMN issued_by_admin_key uuid REFERENCES admin_api_keys(id) ON DELETE SET NULL;
CREATE INDEX api_keys_issued_by_admin_key ON api_keys(issued_by_admin_key) WHERE issued_by_admin_key IS NOT NULL;
