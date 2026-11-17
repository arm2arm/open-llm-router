-- Logical JSON bytes, including a fixed allowance for the row identifiers.
-- Generated values also cover historical rows and direct maintenance writes.
ALTER TABLE request_traces ADD COLUMN stored_bytes bigint GENERATED ALWAYS AS (
    octet_length(detail::text)::bigint + COALESCE(octet_length(summary::text),0)::bigint + 256
) STORED;
CREATE INDEX request_traces_user_storage_idx ON request_traces(user_id) INCLUDE (stored_bytes);
