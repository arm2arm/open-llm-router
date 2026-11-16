package integration_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestStreamEOFClientAndPersistedOutcome(t *testing.T) {
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		ending := r.Header.Get("X-Test-Ending")
		if r.URL.Path == "/v1/messages" {
			io.WriteString(w, "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
			switch ending {
			case "complete":
				io.WriteString(w, "event: message_stop\ndata: {}\n\n")
			case "error":
				io.WriteString(w, "event: error\ndata: {\"error\":{\"message\":\"failed\"}}\n\n")
			}
		} else {
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			switch ending {
			case "complete":
				io.WriteString(w, "data: [DONE]\n\n")
			case "error":
				io.WriteString(w, "data: {\"error\":{\"message\":\"failed\"}}\n\n")
			}
		}
	}))
	want := map[string]string{}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		for ending, status := range map[string]string{"truncated": "aborted", "complete": "200", "error": "error"} {
			req, err := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+path, strings.NewReader(`{"model":"chat-model","stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+f.key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Test-Ending", ending)
			resp, err := f.router.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || (readErr != nil) != (status == "aborted") {
				t.Fatalf("%s/%s: HTTP %d read error=%v", path, ending, resp.StatusCode, readErr)
			}
			want[resp.Header.Get("X-Request-Id")] = status
		}
	}
	f.stop() // Drain asynchronous usage writes before checking persisted outcomes.
	for id, expected := range want {
		var status string
		if err := f.pool.QueryRow(t.Context(), `SELECT status FROM usage WHERE request_id=$1`, id).Scan(&status); err != nil || status != expected {
			t.Fatalf("persisted status=%s want %s: %v", status, expected, err)
		}
	}
}
