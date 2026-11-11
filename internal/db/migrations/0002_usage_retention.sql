-- The retention job deletes per-request records by age.
CREATE INDEX usage_ts_idx ON usage (ts);
