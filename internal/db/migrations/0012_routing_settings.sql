-- Routing (logical models, their endpoints and aliases) becomes a managed
-- settings domain alongside authentication.
ALTER TABLE settings_revisions DROP CONSTRAINT settings_revisions_domain_check;
ALTER TABLE settings_revisions ADD CONSTRAINT settings_revisions_domain_check CHECK (domain IN ('authentication', 'routing'));
ALTER TABLE settings_state DROP CONSTRAINT settings_state_domain_check;
ALTER TABLE settings_state ADD CONSTRAINT settings_state_domain_check CHECK (domain IN ('authentication', 'routing'));
