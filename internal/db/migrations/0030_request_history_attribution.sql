-- Each disjoint attribution path can seek the user's newest rows before merge.
-- The direct partial index also protects service accounts whose many forwarded
-- rows belong to other users and must not be walked for their own history.
CREATE INDEX usage_attributed_forwarded_ts ON usage(forwarded_for,ts DESC) WHERE forwarded_for IS NOT NULL;
CREATE INDEX usage_attributed_direct_ts ON usage(user_id,ts DESC) WHERE forwarded_for IS NULL;
