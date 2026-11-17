-- OIDC refreshes profile columns even when their values have not changed.
-- Only real administrative changes invalidate pending agent previews.
DROP TRIGGER admin_users_changed ON users;
CREATE FUNCTION bump_admin_user_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'UPDATE' THEN
  IF ROW(OLD.groups,OLD.enabled,OLD.protected_admin,OLD.username,OLD.email)
   IS NOT DISTINCT FROM ROW(NEW.groups,NEW.enabled,NEW.protected_admin,NEW.username,NEW.email) THEN
   RETURN NULL;
  END IF;
 END IF;
 UPDATE admin_state SET version=version+1;
 RETURN NULL;
END $$;
CREATE TRIGGER admin_users_changed AFTER INSERT OR DELETE OR UPDATE OF groups,enabled,protected_admin,username,email
 ON users FOR EACH ROW EXECUTE FUNCTION bump_admin_user_state();
