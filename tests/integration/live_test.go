package integration_test

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/requesttrace"
)

type liveStream struct {
	resp   *http.Response
	lines  chan string
	cancel context.CancelFunc
}

func (b *portalBrowser) live(t *testing.T) *liveStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", b.f.router.URL+"/portal/api/requests/events", nil)
	base, _ := url.Parse(b.f.cfg.Portal.BaseURL)
	req.Host = base.Host
	client := *b.client
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &liveStream{resp: resp, lines: make(chan string, 256), cancel: cancel}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	if resp.StatusCode != 200 {
		return s
	}
	go func() {
		defer close(s.lines)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			s.lines <- scanner.Text()
		}
	}()
	s.wait(t, "event: ready")
	return s
}

// wait returns the first line containing every fragment.
func (s *liveStream) wait(t *testing.T, fragments ...string) string {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				t.Fatalf("stream closed while waiting for %v", fragments)
			}
			match := true
			for _, f := range fragments {
				match = match && strings.Contains(line, f)
			}
			if match {
				return line
			}
		case <-timeout:
			t.Fatalf("no event with %v", fragments)
		}
	}
}

func TestLiveRequestEvents(t *testing.T) {
	previous := portal.LiveHeartbeat
	portal.LiveHeartbeat = 300 * time.Millisecond
	t.Cleanup(func() { portal.LiveHeartbeat = previous })
	f := startPortal(t)
	a, b := f.browser(t), f.browser(t)
	alice := a.login(t, "live-alice", []string{"staff"})
	if err := f.traces.Save(t.Context(), alice, requesttrace.Preferences{Enabled: true, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	b.login(t, "live-bob", []string{"staff"})
	if resp, _ := f.browser(t).call(t, "GET", "/portal/api/requests/events", "", ""); resp.StatusCode != 401 {
		t.Fatal("anonymous live stream", resp.StatusCode)
	}
	aliceLive, bobLive := a.live(t), b.live(t)
	key, err := (auth.Admin{Pool: f.pool}).CreateKey(t.Context(), alice, "live-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model","stream":true,"max_tokens":7,"messages":[{"role":"user","content":"live-private-content"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Header.Get("X-Request-Id")
	_, _ = bufio.NewReader(resp.Body).WriteTo(&strings.Builder{})
	resp.Body.Close()
	// Relayed between instances: started, finished with metadata, trace_ready.
	aliceLive.wait(t, `"type":"started"`, id)
	finished := aliceLive.wait(t, `"type":"finished"`, id)
	if !strings.Contains(finished, `"status":"200"`) || !strings.Contains(finished, `"model":"chat-model"`) || strings.Contains(finished, "live-private-content") {
		t.Fatal("finished event", finished)
	}
	aliceLive.wait(t, `"type":"trace_ready"`, id)
	bobLive.wait(t, ": ping")
	select {
	case line := <-bobLive.lines:
		if strings.Contains(line, id) {
			t.Fatal("another user received the event")
		}
	default:
	}
	// Per-user connection limit (one is already open).
	for range 3 {
		if s := a.live(t); s.resp.StatusCode != 200 {
			t.Fatal("refused below the limit", s.resp.StatusCode)
		}
	}
	if s := a.live(t); s.resp.StatusCode != 429 {
		t.Fatal("fifth connection accepted", s.resp.StatusCode)
	}
	// Signing out ends open streams at the next heartbeat.
	if r, _ := a.call(t, "POST", "/portal/logout", `{}`, f.cfg.Portal.BaseURL); r.StatusCode >= 400 {
		t.Fatal("logout", r.StatusCode)
	}
	aliceLive.wait(t, "event: end")
}
