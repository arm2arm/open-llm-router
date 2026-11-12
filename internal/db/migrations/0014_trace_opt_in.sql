-- Never infer consent from the historical default. Preserve explicit saved choices.
ALTER TABLE users ALTER COLUMN trace_enabled SET DEFAULT false;
UPDATE users SET trace_enabled=false WHERE trace_generation=0;
