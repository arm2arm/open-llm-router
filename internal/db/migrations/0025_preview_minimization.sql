-- Preview content is mutable temporary state, distinct from append-only audit.
-- Existing snapshots may contain erased identities and free-form reasons.
UPDATE admin_changes SET summary=jsonb_build_object(
    'operation',operation,
    'content','Historical preview values omitted; prepare a new preview for current before/after details.'
);
CREATE INDEX admin_changes_preview_expiry_idx ON admin_changes(expires) WHERE status='previewed';
CREATE INDEX admin_changes_finished_expiry_idx ON admin_changes((COALESCE(completed,created))) WHERE status<>'previewed';
