-- Small per-trace facts listed without reading the full content document,
-- and a per-user hashed key grouping the requests of one conversation.
ALTER TABLE request_traces ADD COLUMN summary jsonb;
ALTER TABLE request_traces ADD COLUMN conversation_key text;
CREATE INDEX request_traces_conversation_idx ON request_traces (user_id, conversation_key) WHERE conversation_key IS NOT NULL;
