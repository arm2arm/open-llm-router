-- Target filters span several target types; the existing target_type-leading
-- index cannot serve these lookups efficiently.
CREATE INDEX audit_events_target_id ON audit_events(target_id,id DESC);
