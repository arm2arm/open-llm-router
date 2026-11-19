-- Optional display-only claims from a verified institutional login. Identity
-- and permissions continue to use issuer/subject and group policy.
ALTER TABLE users
    ADD COLUMN full_name text NOT NULL DEFAULT '' CHECK (octet_length(full_name) <= 1024),
    ADD COLUMN given_name text NOT NULL DEFAULT '' CHECK (octet_length(given_name) <= 1024),
    ADD COLUMN family_name text NOT NULL DEFAULT '' CHECK (octet_length(family_name) <= 1024),
    ADD COLUMN institution text NOT NULL DEFAULT '' CHECK (octet_length(institution) <= 1024);
