package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestStrictQuotaShapesNeverDispatchUnreservedWork(t *testing.T) {
	const quota = 500
	var calls atomic.Int64
	var f *usageRouter
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			return
		}
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil || fields["max_tokens"] != nil || string(fields["max_completion_tokens"]) != "10" {
			t.Error("forwarded output allowance differs from reserved limit")
		}
		if got := f.currentUsage(t).Reserved; got != int64(len(body))+10 {
			t.Error("in-flight reservation differs from forwarded request", got, len(body))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`)
	})
	f = startUsageRouter(t, backend, usageOptions{group: &config.Group{Models: []string{"*"}, TokensPerDay: limitPtr(int64(quota)), StrictQuota: true}})
	for _, request := range []struct{ path, fields string }{
		{"/v1/chat/completions", `"n":100`},
		{"/v1/completions", `"best_of":100`},
		{"/v1/completions", `"prompt":["a","b"]`},
		{"/v1/chat/completions", `"max_completion_tokens":"1000"`},
		{"/v1/chat/completions", `"max_tokens":"10"`},
		{"/v1/chat/completions", `"max_tokens":null`},
		{"/v1/chat/completions", `"max_tokens":1.1`},
		{"/v1/chat/completions", `"messages":[{"content":[{"type":"image_url","image_url":{"url":"http://media.invalid/image"}}]}]`},
		{"/v1/messages", `"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"http://media.invalid/document"}}]}]`},
		{"/v1/responses", `"max_output_tokens":10,"input":[{"type":"input_image","image_url":"http://media.invalid/image"}]`},
	} {
		body := `{"model":"chat-model","max_tokens":10,` + request.fields + `}`
		if strings.HasPrefix(request.fields, `"max_tokens"`) {
			body = `{"model":"chat-model",` + request.fields + `}`
		}
		response := f.call(t, f.key, "POST", request.path, body)
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal("unbounded request was not refused", request.path, response.StatusCode)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid strict shapes reached the backend", calls.Load())
	}
	accepted, limited := 0, 0
	for range 30 {
		response := f.call(t, f.key, "POST", "/v1/chat/completions", `{"model":"chat-model","messages":[{"role":"user","content":"hello"}],"max_completion_tokens":10,"n":1}`)
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		switch response.StatusCode {
		case 200:
			accepted++
		case 429:
			limited++
		default:
			t.Fatal("valid strict request failed", response.StatusCode)
		}
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == uint64(accepted) })
	var used int64
	if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(input_tokens+output_tokens),0) FROM usage_daily WHERE user_id=$1`, f.owner).Scan(&used); err != nil || used > quota || used != int64(accepted*30) || accepted == 0 || limited == 0 || calls.Load() != int64(accepted) {
		t.Fatal("strict daily quota exceeded or unreserved work dispatched", used, accepted, limited, err)
	}
	if snapshot := f.currentUsage(t); snapshot.Reserved != 0 || snapshot.InFlight != 0 {
		t.Fatal("strict shape validation leaked reservations")
	}
}
