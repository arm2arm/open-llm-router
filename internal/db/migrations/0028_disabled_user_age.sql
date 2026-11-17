ALTER TABLE users ADD COLUMN disabled_at timestamptz;
-- Historical disable times are unknown: start their age at this upgrade rather
-- than incorrectly aging them from their last identity sync.
UPDATE users SET disabled_at=clock_timestamp() WHERE NOT enabled;
CREATE FUNCTION track_user_disable_time() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.enabled THEN
        NEW.disabled_at := NULL;
    ELSIF TG_OP='INSERT' OR OLD.enabled THEN
        NEW.disabled_at := clock_timestamp();
    ELSE
        NEW.disabled_at := OLD.disabled_at;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER user_disable_time BEFORE INSERT OR UPDATE OF enabled ON users
    FOR EACH ROW EXECUTE FUNCTION track_user_disable_time();
ALTER TABLE users ADD CONSTRAINT disabled_user_timestamp CHECK
    ((enabled AND disabled_at IS NULL) OR (NOT enabled AND disabled_at IS NOT NULL));
CREATE INDEX users_disabled_age ON users(disabled_at,id) WHERE NOT enabled;
