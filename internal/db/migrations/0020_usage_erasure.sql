-- The service that forwarded a call is optional provenance. The end user's
-- history and daily charges survive erasure of that service account.
ALTER TABLE usage ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE usage DROP CONSTRAINT usage_user_id_fkey;
ALTER TABLE usage ADD CONSTRAINT usage_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL;

-- Remove direct calls before clearing their owner reference. The existing
-- forwarded_for CASCADE still erases every call attributed to an end user.
CREATE FUNCTION erase_direct_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM usage WHERE user_id=OLD.id AND forwarded_for IS NULL;
    RETURN OLD;
END
$$;
CREATE TRIGGER users_erase_direct_usage BEFORE DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION erase_direct_usage();
ALTER TABLE usage ADD CONSTRAINT usage_attributed_user
    CHECK (user_id IS NOT NULL OR forwarded_for IS NOT NULL);
