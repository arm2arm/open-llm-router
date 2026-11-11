package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/reqid"
)

// wait is how long a test waits for something that should happen at once.
const wait = 5 * time.Second

// logBuffer collects log output from the server's goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines decodes the JSON log lines written so far.
func (b *logBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, entry)
	}
	return out
}

func newTestServer() (*Server, *logBuffer) {
	logs := &logBuffer{}
	return New(slog.New(slog.NewJSONHandler(logs, nil))), logs
}

func TestHealthEndpoints(t *testing.T) {
	s, logs := newTestServer()
	for path, want := range map[string]string{"/healthz": `{"status":"ok"}`, "/readyz": `{"status":"ready"}`} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("GET %s = %d %q, want 200 %s", path, rec.Code, rec.Body.String(), want)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: Content-Type = %q", path, ct)
		}
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", rec.Code)
	}
	// Probes arrive every few seconds; only a failing one is worth a log line.
	if got := logs.lines(t); len(got) != 1 || got[0]["status"] != float64(405) {
		t.Errorf("want one log line, for the 405; got %v", got)
	}
}

func TestReadinessContext(t *testing.T) {
	s, _ := newTestServer()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.SetReadiness(func(ctx context.Context) bool {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > readinessTimeout {
			t.Error("readiness check has no bounded deadline")
		}
		if ctx.Err() != context.Canceled {
			t.Error("request cancellation did not reach readiness check")
		}
		return false
	})
	for path, want := range map[string]int{"/readyz": http.StatusServiceUnavailable, "/healthz": http.StatusOK} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, "GET", path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

func TestRequestIDAndLog(t *testing.T) {
	s, logs := newTestServer()
	var inHandler string
	s.Handle("POST /v1/echo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inHandler = reqid.FromContext(r.Context())
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "the response text")
	}))

	req := httptest.NewRequest("POST", "/v1/echo?api_key=olr-querysecret", strings.NewReader("the prompt text"))
	req.Header.Set("Authorization", "Bearer olr-headersecret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	id := rec.Header().Get("X-Request-Id")
	if len(id) != 32 {
		t.Fatalf("X-Request-Id = %q, want 32 hex characters", id)
	}
	if inHandler != id {
		t.Errorf("request ID in the handler's context = %q, in the response header = %q", inHandler, id)
	}

	entries := logs.lines(t)
	if len(entries) != 1 {
		t.Fatalf("want one log line, got %d: %s", len(entries), logs)
	}
	e := entries[0]
	if e["request_id"] != id || e["method"] != "POST" || e["path"] != "/v1/echo" || e["status"] != float64(201) || e["bytes"] != float64(len("the response text")) {
		t.Errorf("log line = %v", e)
	}
	for _, private := range []string{"the prompt text", "the response text", "querysecret", "headersecret"} {
		if strings.Contains(logs.String(), private) {
			t.Errorf("log contains %q: %s", private, logs)
		}
	}

	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, httptest.NewRequest("GET", "/nowhere", nil))
	if id2 := rec2.Header().Get("X-Request-Id"); id2 == id || id2 == "" {
		t.Errorf("second request ID = %q, first = %q", id2, id)
	}
}

func TestLogStatusOfAnEmptyResponse(t *testing.T) {
	s, logs := newTestServer()
	s.Handle("GET /empty", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/empty", nil))
	if got := logs.lines(t); len(got) != 1 || got[0]["status"] != float64(200) {
		t.Errorf("a handler that writes nothing sends 200; log = %v", got)
	}
}

// A streamed response must reach the client chunk by chunk through the
// wrapper that records status and size, whichever way the handler flushes.
func TestStreamingPassesThrough(t *testing.T) {
	flushers := map[string]func(http.ResponseWriter) error{
		"http.Flusher": func(w http.ResponseWriter) error {
			w.(http.Flusher).Flush()
			return nil
		},
		"http.ResponseController": func(w http.ResponseWriter) error {
			return http.NewResponseController(w).Flush()
		},
	}
	for name, flush := range flushers {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestServer()
			release := make(chan struct{})
			s.Handle("GET /stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, "first\n")
				if err := flush(w); err != nil {
					t.Errorf("flush: %v", err)
				}
				<-release
				io.WriteString(w, "last\n")
			}))
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			resp, err := ts.Client().Get(ts.URL + "/stream")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body := bufio.NewReader(resp.Body)

			first := make(chan string, 1)
			go func() {
				line, _ := body.ReadString('\n')
				first <- line
			}()
			select {
			case line := <-first:
				if line != "first\n" {
					t.Fatalf("first chunk = %q", line)
				}
			case <-time.After(wait):
				close(release)
				t.Fatal("the first chunk did not arrive while the handler was still running")
			}
			close(release)
			if rest, _ := io.ReadAll(body); string(rest) != "last\n" {
				t.Errorf("rest of the stream = %q", rest)
			}
		})
	}
}

func TestPanicBeforeResponse(t *testing.T) {
	s, logs := newTestServer()
	s.Handle("GET /boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "kaboom") {
		t.Errorf("the response shows the panic value: %q", rec.Body)
	}
	entries := logs.lines(t)
	if len(entries) != 2 {
		t.Fatalf("want a panic line and a request line, got: %s", logs)
	}
	id := rec.Header().Get("X-Request-Id")
	if entries[0]["level"] != "ERROR" || entries[0]["panic"] != "kaboom" || entries[0]["request_id"] != id {
		t.Errorf("panic line = %v", entries[0])
	}
	if entries[1]["status"] != float64(500) || entries[1]["request_id"] != id {
		t.Errorf("request line = %v", entries[1])
	}
}

// After the response has started, a panic cannot become a 500 any more. The
// connection must break, so the client does not take the stream for complete.
func TestPanicMidStreamBreaksTheConnection(t *testing.T) {
	s, logs := newTestServer()
	s.Handle("GET /boom", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		panic("kaboom")
	}))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ts.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)

	resp, err := ts.Client().Get(ts.URL + "/boom")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("the body %q ended without an error; the client cannot tell it is incomplete", body)
	}
	if !strings.Contains(logs.String(), `"panic":"kaboom"`) {
		t.Errorf("the panic was not logged: %s", logs)
	}
}

// run starts s.Run on a free local port and returns the address, the function
// that plays the role of SIGTERM, and the channel with Run's result.
func run(t *testing.T, s *Server, grace time.Duration) (addr string, stop context.CancelFunc, result <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln, grace) }()
	return ln.Addr().String(), cancel, done
}

type response struct {
	body string
	err  error
}

// get requests url in the background on a connection of its own.
func get(url string) <-chan response {
	out := make(chan response, 1)
	go func() {
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := client.Get(url)
		if err != nil {
			out <- response{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		out <- response{string(body), err}
	}()
	return out
}

func TestShutdownLetsRequestsInFlightFinish(t *testing.T) {
	s, logs := newTestServer()
	started, release := make(chan struct{}), make(chan struct{})
	s.Handle("GET /slow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "complete")
	}))
	addr, stop, result := run(t, s, wait)

	resp := get("http://" + addr + "/slow")
	<-started
	stop()

	// New connections are refused as soon as shutdown begins ...
	refused := false
	for deadline := time.Now().Add(wait); time.Now().Before(deadline) && !refused; time.Sleep(5 * time.Millisecond) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			refused = true
		} else {
			conn.Close()
		}
	}
	if !refused {
		t.Error("the server still accepts connections after shutdown began")
	}
	// ... while the request in flight keeps the server alive.
	select {
	case err := <-result:
		t.Fatalf("Run returned (%v) while a request was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case r := <-resp:
		if r.err != nil || r.body != "complete" {
			t.Errorf("request in flight got %q, %v; want the complete response", r.body, r.err)
		}
	case <-time.After(wait):
		t.Fatal("the request in flight never finished")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("Run = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(wait):
		t.Fatal("Run did not return after the last request finished")
	}
	if !strings.Contains(logs.String(), `"msg":"shutting down"`) {
		t.Errorf("shutdown was not logged: %s", logs)
	}
}

func TestShutdownCutsRequestsOffAfterTheGracePeriod(t *testing.T) {
	s, _ := newTestServer()
	started, cancelled := make(chan struct{}), make(chan struct{})
	s.Handle("GET /stuck", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // what a proxied request waits on; ends the upstream call
		close(cancelled)
	}))
	addr, stop, result := run(t, s, 50*time.Millisecond)

	resp := get("http://" + addr + "/stuck")
	<-started
	stop()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "grace period") {
			t.Errorf("Run = %v, want an error about the grace period", err)
		}
	case <-time.After(wait):
		t.Fatal("Run did not return after the grace period")
	}
	select {
	case <-cancelled:
	case <-time.After(wait):
		t.Error("the context of the request that was cut off was not cancelled")
	}
	select {
	case r := <-resp:
		if r.err == nil {
			t.Errorf("the client got a response (%q) from a request that was cut off", r.body)
		}
	case <-time.After(wait):
		t.Error("the client's connection was not closed")
	}
}

func TestRunReportsAListenerThatFails(t *testing.T) {
	s, _ := newTestServer()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if err := s.Run(context.Background(), ln, wait); err == nil {
		t.Error("Run on a closed listener returned nil")
	}
}
