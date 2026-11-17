-- Month lookups and the user-deletion foreign key need the user leading.
CREATE INDEX usage_daily_user_day_idx ON usage_daily(user_id,day);
