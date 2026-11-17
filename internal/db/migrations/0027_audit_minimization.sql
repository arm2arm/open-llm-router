-- Administration attribution needs a network, not a precise host address.
CREATE FUNCTION audit_source_network(value text) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT AS $$
DECLARE address inet;
BEGIN
    address := value::inet;
    IF family(address) = 6 AND address <<= '::ffff:0.0.0.0/96'::inet THEN
        address := regexp_replace(host(address), '^::ffff:', '', 'i')::inet;
    END IF;
    RETURN network(set_masklen(address, CASE WHEN family(address)=4 THEN 24 ELSE 48 END))::text;
EXCEPTION WHEN invalid_text_representation THEN
    RETURN NULL;
END;
$$;

CREATE FUNCTION minimize_audit_source() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE source text;
BEGIN
    IF jsonb_typeof(NEW.details->'agent')='object' AND NEW.details->'agent' ? 'source_ip' THEN
        source := audit_source_network(NEW.details#>>'{agent,source_ip}');
        IF source IS NULL THEN
            NEW.details := NEW.details #- '{agent,source_ip}';
        ELSE
            NEW.details := jsonb_set(NEW.details,'{agent,source_ip}',to_jsonb(source));
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER audit_source_minimization BEFORE INSERT ON audit_events
    FOR EACH ROW EXECUTE FUNCTION minimize_audit_source();

CREATE FUNCTION minimize_admin_key_source() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.last_ip := audit_source_network(NEW.last_ip);
    RETURN NEW;
END;
$$;
CREATE TRIGGER admin_key_source_minimization BEFORE INSERT OR UPDATE OF last_ip ON admin_api_keys
    FOR EACH ROW EXECUTE FUNCTION minimize_admin_key_source();
UPDATE admin_api_keys SET last_ip=audit_source_network(last_ip) WHERE last_ip IS NOT NULL;

-- One-time transactional privacy repair. Restore the append-only guard before
-- this migration commits; failure rolls back both data and trigger changes.
ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only;
UPDATE audit_events SET details=CASE
    WHEN audit_source_network(details#>>'{agent,source_ip}') IS NULL THEN details #- '{agent,source_ip}'
    ELSE jsonb_set(details,'{agent,source_ip}',to_jsonb(audit_source_network(details#>>'{agent,source_ip}')))
END WHERE jsonb_typeof(details->'agent')='object' AND details->'agent' ? 'source_ip';
ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only;
