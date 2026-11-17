-- trace_generation also changed when a user erased content, so it cannot prove
-- that an enabled legacy account made an explicit capture choice. Do not invent
-- consent dates: require a new choice for all pre-provenance enabled accounts.
ALTER TABLE users ADD COLUMN trace_preferences_set_at timestamptz;
UPDATE users SET trace_enabled=false, trace_generation=trace_generation+1 WHERE trace_enabled;
