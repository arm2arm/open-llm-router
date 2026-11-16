package integration_test

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestResponsesStrictQuotaAndPersistence(t *testing.T) {
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":20}}`)
	}), usageOptions{group: &config.Group{Models: []string{"*"}, TokensPerDay: limitPtr(int64(1000)), StrictQuota: true}})
	request := `{"model":"chat-model","input":"hello world","max_output_tokens":20}`
	resp := f.call(t, f.key, "POST", "/v1/responses", request)
	if resp.StatusCode != 200 {
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	waitFor(t, func() bool { return f.currentUsage(t).Used.Today == 30 })
	requireLimit(t, f.call(t, f.key, "POST", "/v1/responses", strings.Replace(request, ":20}", ":1000}", 1)), "tokens_per_day", false)
	resp = f.call(t, f.key, "POST", "/v1/responses", `{"model":"chat-model","input":"hello"}`)
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(out), "max_output_tokens") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	resp = f.call(t, f.key, "POST", "/v1/responses", `{"model":"chat-model","input":"hello","max_tokens":1}`)
	out, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(out), "max_output_tokens") {
		t.Fatalf("legacy output field bypassed Responses admission: %d %s", resp.StatusCode, out)
	}
	f.stop()
	var input, output int64
	if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0) FROM usage WHERE user_id=$1 AND operation='responses'`, f.owner).Scan(&input, &output); err != nil {
		t.Fatal(err)
	}
	if input != 10 || output != 20 {
		t.Fatalf("persisted usage %d %d", input, output)
	}
}

func TestInterruptedResponsesInputChargesTPM(t *testing.T) {
	body := `{"model":"chat-model","input":"` + strings.Repeat("x", 1200) + `","stream":true,"store":false,"max_output_tokens":1}`
	charge := int64(len(body) + 1)
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}), usageOptions{group: &config.Group{Models: []string{"*"}, TPM: limitPtr(charge)}, clock: func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }})
	resp := f.call(t, f.key, "POST", "/v1/responses", body)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitFor(t, func() bool { return f.currentUsage(t).Used.Today == charge })
	requireLimit(t, f.call(t, f.key, "POST", "/v1/responses", body), "tpm", false)
	f.stop()
	var recorded, daily int64
	if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(input_tokens),0) FROM usage WHERE user_id=$1`, f.owner).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(input_tokens),0) FROM usage_daily WHERE user_id=$1`, f.owner).Scan(&daily); err != nil {
		t.Fatal(err)
	}
	if recorded != int64(len(body)) || daily != recorded {
		t.Fatalf("input differs: records=%d daily=%d expected=%d", recorded, daily, len(body))
	}
}
