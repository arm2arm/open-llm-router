package integration_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

func TestUsageTimingPreservesStrictQuotaAndPersistence(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses=%v/stream=%v", responses, stream), func(t *testing.T) {
				path, limit := "/v1/chat/completions", "max_tokens"
				wire := `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":5}}}`
				if responses {
					path, limit = "/v1/responses", "max_output_tokens"
					wire = `{"status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":20,"output_tokens_details":{"reasoning_tokens":5}}}`
				}
				if stream {
					if responses {
						wire = "data: {\"type\":\"response.completed\",\"response\":" + wire + "}\n\n"
					} else {
						wire = "data: " + wire + "\n\ndata: [DONE]\n\n"
					}
				}
				f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if backendProbe(r) {
						return
					}
					kind := "application/json"
					if stream {
						kind = "text/event-stream"
					}
					w.Header().Set("Content-Type", kind)
					_, _ = io.WriteString(w, wire)
				}), usageOptions{group: &config.Group{Models: []string{"*"}, TokensPerDay: limitPtr(int64(1000)), StrictQuota: true}})
				body := fmt.Sprintf(`{"model":"alias","messages":[],"input":"hello","%s":20,"stream":%v,"stream_options":{"include_usage":true}}`, limit, stream)
				req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+f.key)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(usage.TimingHeader, "true")
				resp, err := f.router.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				out, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || !strings.Contains(string(out), `"olr_observed_output_tokens_per_second":`) {
					t.Fatalf("%d %s %v", resp.StatusCode, out, err)
				}
				waitFor(t, func() bool { return f.currentUsage(t).Used.Today == 30 })
				requireLimit(t, f.call(t, f.key, "POST", path, strings.Replace(body, `":20`, `":1000`, 1)), "tokens_per_day", false)
				f.stop()
				var input, output int64
				if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0) FROM usage WHERE user_id=$1`, f.owner).Scan(&input, &output); err != nil {
					t.Fatal(err)
				}
				if input != 10 || output != 20 {
					t.Fatalf("timing changed persisted charges: input=%d output=%d", input, output)
				}
			})
		}
	}
}
