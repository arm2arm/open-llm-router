-- Preserve display addresses, but do not assume pre-existing SSO claims were
-- verified. A fresh verified login/access token makes them eligible again.
-- Local addresses are explicitly assigned by an operator and are trusted by
-- the forwarder independently of this OIDC claim flag.
ALTER TABLE users ADD COLUMN email_verified boolean NOT NULL DEFAULT false;
