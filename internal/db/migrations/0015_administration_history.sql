-- Search and stable cursor pagination for administration history.
CREATE INDEX audit_events_action ON audit_events(action, id DESC);
CREATE INDEX audit_events_actor ON audit_events(actor_id, id DESC);

-- Legacy endpoint revisions now obey the same append-only contract as complete
-- routing revisions. Attribution survives account erasure without an FK rewrite.
ALTER TABLE endpoint_revisions DROP CONSTRAINT endpoint_revisions_created_by_fkey;
CREATE TRIGGER endpoint_revisions_append_only BEFORE UPDATE OR DELETE ON endpoint_revisions
    FOR EACH ROW EXECUTE FUNCTION forbid_history_change();
CREATE TRIGGER endpoint_revisions_no_truncate BEFORE TRUNCATE ON endpoint_revisions
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_history_change();
