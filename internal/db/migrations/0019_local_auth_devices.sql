-- A device cookie grants a separate guessing budget, never a session or access.
CREATE TABLE local_auth_devices (
    hash bytea PRIMARY KEY CHECK (octet_length(hash)=32),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires timestamptz NOT NULL
);
CREATE INDEX local_auth_devices_owner ON local_auth_devices(user_id,created);
CREATE INDEX local_auth_devices_expiry ON local_auth_devices(expires);

-- Anonymous clients cannot fill the known-device bucket namespace.
ALTER TABLE portal_auth_throttle ADD COLUMN known_device boolean NOT NULL DEFAULT false;
