-- Older captures used one flag for input and output bounds. Conservatively mark
-- limited legacy output as unknown, including list summaries read without detail.
WITH limited AS (
    SELECT request_id,
        COALESCE(summary,'{}'::jsonb) || jsonb_build_object(
            'tool_calls',NULL,
            'observed_tool_calls',COALESCE(summary->'tool_calls','0'::jsonb),
            'finish_reason','') AS new_summary
    FROM request_traces
    WHERE detail->>'truncated'='true' AND NOT detail ? 'output_truncated'
)
UPDATE request_traces t SET summary=l.new_summary,
    detail=jsonb_set(jsonb_set(t.detail,'{output_truncated}','true'::jsonb),'{summary}',l.new_summary)
FROM limited l WHERE t.request_id=l.request_id;
