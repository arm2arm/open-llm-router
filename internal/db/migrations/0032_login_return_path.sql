-- Bind the post-login destination to the same one-time, browser-bound attempt
-- as the OIDC state and PKCE verifier. Never trust a callback query parameter.
ALTER TABLE portal_login_attempts ADD COLUMN return_to text NOT NULL DEFAULT '/portal/';
