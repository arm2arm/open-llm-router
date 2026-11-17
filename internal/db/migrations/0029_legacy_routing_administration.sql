-- Revisions predating managed groups use deployment policies. A missing/null
-- groups member cannot be interpreted as an explicit removal of all admins.
CREATE OR REPLACE FUNCTION invalidate_admin_routing() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE active_document jsonb;
BEGIN
 IF NEW.domain='routing' AND (TG_OP='INSERT' OR OLD.active_revision IS DISTINCT FROM NEW.active_revision) THEN
  SELECT document INTO active_document FROM settings_revisions WHERE id=NEW.active_revision;
  IF jsonb_typeof(active_document->'groups') IS DISTINCT FROM 'object' THEN
   RETURN NEW;
  END IF;
  WITH revoked_keys AS (
   UPDATE admin_api_keys k SET revoked=true,policy_version=policy_version+1
   FROM users u WHERE k.owner_id=u.id AND NOT k.revoked AND NOT u.protected_admin
   AND NOT EXISTS (SELECT 1 FROM unnest(u.groups) AS membership(name)
    WHERE COALESCE((active_document->'groups'->membership.name->>'admin')::boolean,false))
   RETURNING k.id,k.owner_id
  )
  INSERT INTO audit_events(action,actor_type,actor_name,target_type,target_id,revision,result,reason,details)
  SELECT 'admin_key.revoke','system','routing-policy','admin_key',id::text,NEW.active_revision,
   'succeeded','owner no longer has administration authority',jsonb_build_object('owner_id',owner_id)
  FROM revoked_keys;
 END IF;
 RETURN NEW;
END $$;
