package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProxyTracePreservesHiddenTerminalUsageAfterOutputLimit(t *testing.T) {
	var traces *requesttrace.Service
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if backendProbe(r) {
			w.WriteHeader(200)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 3000 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ordinary token %d \"}}]}\n\n", i)
		}
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"late-tool\",\"function\":{\"name\":\"weather\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3000,\"prompt_tokens_details\":{\"cached_tokens\":7},\"completion_tokens_details\":{\"reasoning_tokens\":9}}}\n\ndata: [DONE]\n\n")
	}), usageOptions{configure: func(_ *config.Config, p *proxy.Handler, _ *server.Server, pool *pgxpool.Pool) {
		var err error
		traces, err = requesttrace.New(t.Context(), pool, slog.New(slog.NewTextHandler(io.Discard, nil)), config.ContentTracing{Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(traces.Close)
		p.Traces = traces
	}})
	if err := traces.Save(t.Context(), f.owner, requesttrace.Preferences{Enabled: true, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	r := f.call(t, f.key, "POST", "/v1/chat/completions", `{"model":"chat-model","stream":true,"max_tokens":4000,"messages":[]}`)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || r.StatusCode != 200 || !strings.Contains(string(body), "late-tool") || strings.Contains(string(body), `"usage"`) {
		t.Fatal("client response changed", r.StatusCode, err)
	}
	waitFor(t, func() bool { return traces.Processed.Load() == 1 })
	var detail requesttrace.Detail
	var summary requesttrace.Summary
	var detailJSON, summaryJSON []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT detail,summary FROM request_traces WHERE request_id=$1`, r.Header.Get("X-Request-Id")).Scan(&detailJSON, &summaryJSON); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(detailJSON, &detail) != nil || json.Unmarshal(summaryJSON, &summary) != nil {
		t.Fatal("invalid stored JSON")
	}
	if !detail.OutputTruncated || summary.ToolCalls != nil || summary.FinishReason != "" || summary.CachedTokens != 7 || summary.ReasoningTokens != 9 || detail.Result.Usage["completion_tokens"] != 3000 || traces.Failed.Load() != 0 {
		t.Fatal("trace lost terminal usage or invented complete output facts", summary, detail.Result.Usage)
	}
}

func TestLimitedLegacySummariesBecomeUnknownOnUpgrade(t *testing.T) {
	database := newDatabase(t)
	legacy, err := pgxpool.New(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(legacy.Close)
	execSQL(t, legacy, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,name text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`)
	for _, migration := range db.Migrations()[:22] {
		execSQL(t, legacy, migration.SQL)
		execSQL(t, legacy, `INSERT INTO schema_migrations(version,name) VALUES($1,$2)`, migration.Version, migration.Name)
	}
	id := user(t, legacy, "legacy", "limited-output")
	execSQL(t, legacy, `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail,summary) VALUES('legacy-limited',$1,now(),now()+interval '1 day','{"v":3,"truncated":true,"output":[]}', '{"tool_calls":0,"truncated":true,"finish_reason":"stop"}')`, id)
	upgraded := openDatabase(t, database.dsn)
	var raw []byte
	if err := upgraded.QueryRow(t.Context(), `SELECT summary FROM request_traces WHERE request_id='legacy-limited'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var summary requesttrace.Summary
	if json.Unmarshal(raw, &summary) != nil || summary.ToolCalls != nil || summary.FinishReason != "" {
		t.Fatal("legacy summary still claims complete zero", string(raw))
	}
}
