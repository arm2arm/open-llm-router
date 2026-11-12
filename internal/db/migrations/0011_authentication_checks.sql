-- Connection checks and test sign-in results for settings revisions. Like
-- validation, checks is a mutable result column: the immutability guard
-- covers the document and metadata only. Results hold statuses and redacted
-- messages, never secrets or personal claims.
ALTER TABLE settings_revisions ADD COLUMN checks jsonb CHECK (checks IS NULL OR jsonb_typeof(checks) = 'object');

-- A test sign-in reuses the one-use login challenge table but never creates
-- a session: test_revision names the settings revision under test and
-- test_user the administrator who started it.
ALTER TABLE portal_login_attempts ADD COLUMN test_revision bigint REFERENCES settings_revisions(id);
ALTER TABLE portal_login_attempts ADD COLUMN test_user uuid REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE portal_login_attempts ADD CONSTRAINT portal_login_attempt_test
    CHECK ((test_revision IS NULL) = (test_user IS NULL));
