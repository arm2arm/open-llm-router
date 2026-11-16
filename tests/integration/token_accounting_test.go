package integration_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestInterruptedOpenAIInputChargesQuotaAndTPM(t *testing.T) {
	body := `{"model":"chat-model","messages":[{"role":"user","content":"` + strings.Repeat("x", 1200) + `"}],"max_tokens":1,"stream":true,"stream_options":{"include_usage":true}}`
	input := int64(len(body))
	charge := input + 1
	for _, gate := range []string{"daily", "strict_daily", "tpm", "completed_daily"} {
		t.Run(gate, func(t *testing.T) {
			group := config.Group{Models: []string{"*"}, TokensPerDay: limitPtr(2 * charge)}
			count := 2
			if gate == "strict_daily" {
				group.StrictQuota = true
			}
			if gate == "tpm" {
				group.TokensPerDay = nil
				group.TPM = limitPtr(charge)
				count = 1
			}
			f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					return
				}
				raw, _ := io.ReadAll(r.Body)
				if len(raw) != int(input) {
					t.Error("unexpected forwarded input size")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
				w.(http.Flusher).Flush()
				if gate == "completed_daily" {
					_, _ = fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n", input)
					return
				}
				<-r.Context().Done()
			}), usageOptions{group: &group, clock: func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }})
			for i := 1; i <= count; i++ {
				resp := f.call(t, f.key, "POST", "/v1/chat/completions", body)
				if resp.StatusCode != 200 {
					t.Fatal("call", resp.StatusCode)
				}
				if gate == "completed_daily" {
					_, _ = io.Copy(io.Discard, resp.Body)
				} else {
					if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
						t.Fatal(err)
					}
				}
				resp.Body.Close()
				want := int64(i) * charge
				waitFor(t, func() bool { return f.currentUsage(t).Used.Today == want })
			}
			limit := "tokens_per_day"
			if gate == "tpm" {
				limit = "tpm"
			}
			requireLimit(t, f.call(t, f.key, "POST", "/v1/chat/completions", body), limit, false)
			s := f.currentUsage(t)
			if gate == "tpm" && (s.Remaining.TPM == nil || *s.Remaining.TPM != 0) {
				t.Fatal("TPM did not debit input", s)
			}
			f.stop()
			var recorded, daily int64
			if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(input_tokens),0) FROM usage WHERE user_id=$1`, f.owner).Scan(&recorded); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(input_tokens),0) FROM usage_daily WHERE user_id=$1`, f.owner).Scan(&daily); err != nil {
				t.Fatal(err)
			}
			if recorded != input*int64(count) || daily != recorded {
				t.Fatalf("input charge differs: records=%d daily=%d expected=%d", recorded, daily, input*int64(count))
			}
		})
	}
}

func TestCachedInputProtocolParity(t *testing.T) {
	for _, wire := range []struct{ path, body, kind string }{
		{"/v1/chat/completions", `{"usage":{"prompt_tokens":3510,"completion_tokens":7}}`, "application/json"},
		{"/v1/messages", `{"usage":{"input_tokens":10,"cache_read_input_tokens":3000,"cache_creation_input_tokens":500,"output_tokens":7}}`, "application/json"},
		{"/v1/messages", "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":12}}}\n\nevent: message_delta\ndata: {\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":3000,\"cache_creation_input_tokens\":500,\"output_tokens\":7}}\n\nevent: message_stop\ndata: {}\n\n", "text/event-stream"},
	} {
		t.Run(wire.path+wire.kind, func(t *testing.T) {
			f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					return
				}
				w.Header().Set("Content-Type", wire.kind)
				_, _ = io.WriteString(w, wire.body)
			}), usageOptions{group: &config.Group{Models: []string{"*"}, TokensPerDay: limitPtr(int64(3517))}})
			request := `{"model":"chat-model","messages":[],"max_tokens":7}`
			resp := f.call(t, f.key, "POST", wire.path, request)
			if resp.StatusCode != 200 {
				t.Fatal(resp.StatusCode)
			}
			out, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(out) != wire.body {
				t.Fatal("response changed", err)
			}
			waitFor(t, func() bool { return f.currentUsage(t).Used.Today == 3517 })
			requireLimit(t, f.call(t, f.key, "POST", wire.path, request), "tokens_per_day", strings.Contains(wire.path, "messages"))
			f.stop()
			var input int64
			if err := f.pool.QueryRow(t.Context(), `SELECT sum(input_tokens) FROM usage WHERE user_id=$1`, f.owner).Scan(&input); err != nil || input != 3510 {
				t.Fatal("persisted cache input", input, err)
			}
		})
	}
}
