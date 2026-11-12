package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/jackc/pgx/v5/pgxpool"
)

func bodyRequest(ctx context.Context, user, class string, body io.ReadCloser) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Body, r.ContentLength = body, -1
	return r.WithContext(auth.NewContext(ctx, auth.Principal{UserID: user, KeyClass: class, Groups: []string{"staff"}}))
}

func assertBodySlotsReleased(t *testing.T, h *Handler) {
	t.Helper()
	h.bodies.mu.Lock()
	defer h.bodies.mu.Unlock()
	if h.bodies.active != 0 || len(h.bodies.users) != 0 {
		t.Fatalf("leaked upload slots: active=%d users=%v", h.bodies.active, h.bodies.users)
	}
}

// A reader generates bytes on demand so rejected requests cannot hide body
// allocations in the test client. It stalls just below the size limit.
type stalledUpload struct {
	remaining int
	ctx       context.Context
	ready     chan struct{}
	read      atomic.Int64
}

func (b *stalledUpload) Read(p []byte) (int, error) {
	if b.remaining > 0 {
		n := min(len(p), b.remaining)
		for i := range p[:n] {
			p[i] = ' '
		}
		b.remaining -= n
		b.read.Add(int64(n))
		return n, nil
	}
	close(b.ready)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (*stalledUpload) Close() error { return nil }

func TestConcurrentLargeUploadsHaveFixedMemoryCeiling(t *testing.T) {
	one := 1
	cfg := testConfig()
	cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}, MaxConcurrent: &one}}
	h := New(cfg)
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	first := &stalledUpload{remaining: MaxBody - 1, ctx: ctx, ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.forward(httptest.NewRecorder(), bodyRequest(ctx, "user", "interactive", first), config.ProtocolOpenAI)
	}()
	select {
	case <-first.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("first upload did not reach the buffering barrier")
	}
	// Different keys for one principal must not buy additional upload slots.
	var wg sync.WaitGroup
	var bytesRead atomic.Int64
	for i := range 128 {
		wg.Go(func() {
			b := &stalledUpload{remaining: MaxBody - 1, ctx: ctx, ready: make(chan struct{})}
			w := httptest.NewRecorder()
			class := "interactive"
			if i%2 == 0 {
				class = "batch"
			}
			r := bodyRequest(ctx, "user", class, b)
			p, _ := auth.FromContext(r.Context())
			p.KeyID = fmt.Sprint(i)
			r = r.WithContext(auth.NewContext(r.Context(), p))
			h.forward(w, r, config.ProtocolOpenAI)
			bytesRead.Add(b.read.Load())
			if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
				t.Errorf("upload burst: %d %s", w.Code, w.Body.String())
			}
		})
	}
	wg.Wait()
	if bytesRead.Load() != 0 || first.read.Load() != MaxBody-1 {
		t.Fatalf("excess uploads read %d bytes; first read %d", bytesRead.Load(), first.read.Load())
	}
	h.bodies.mu.Lock()
	active, users := h.bodies.active, len(h.bodies.users)
	h.bodies.mu.Unlock()
	if active != 1 || users != 1 {
		t.Fatalf("reader ceiling: active=%d users=%d", active, users)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	// Allow ReadAll growth and runtime/test overhead, while ruling out a
	// body-sized allocation per rejected request (over 512 MiB in this burst).
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth > 4*MaxBody+(8<<20) {
		t.Fatalf("upload heap grew by %d bytes", growth)
	}
	t.Logf("129 uploads: 1 body reader, %d buffered bytes, %d bytes live heap growth", first.read.Load(), growth)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled reader did not exit")
	}
	assertBodySlotsReleased(t, h)
}

func TestBodyAdmissionGlobalAndUserLimits(t *testing.T) {
	b := &bodyAdmission{maximum: 2, perUser: 1, users: make(map[string]int)}
	a, _ := b.acquire("a", nil)
	if slot, gate := b.acquire("a", nil); slot != nil || gate != "body_readers_per_user" {
		t.Fatal("user limit did not apply")
	}
	c, _ := b.acquire("c", nil)
	if slot, gate := b.acquire("d", nil); slot != nil || gate != "body_readers" {
		t.Fatal("global limit did not apply")
	}
	a.release()
	a.release()
	c.release()
	if b.active != 0 || len(b.users) != 0 {
		t.Fatal("slots or user entries leaked")
	}
}

// A failed request must free both slots, including failures after buffering.
func TestBodySlotsReleasedOnErrors(t *testing.T) {
	db, err := pgxpool.New(t.Context(), "postgres://localhost/unused")
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // deterministic admission failure without a database service

	for _, tc := range []struct {
		name   string
		body   io.ReadCloser
		length int64
		status int
	}{
		{"declared oversized", io.NopCloser(strings.NewReader("")), MaxBody + 1, 413},
		{"chunked oversized", io.NopCloser(io.LimitReader(zeroReader{}, MaxBody+1)), -1, 413},
		{"unreadable", io.NopCloser(errorReader{io.ErrUnexpectedEOF}), -1, 400},
		{"timeout", io.NopCloser(errorReader{os.ErrDeadlineExceeded}), -1, 408},
		{"malformed", io.NopCloser(strings.NewReader("{")), -1, 400},
		{"unknown model", io.NopCloser(strings.NewReader(`{"model":"missing"}`)), -1, 404},
		{"admission failure", io.NopCloser(strings.NewReader(`{"model":"chat-model"}`)), -1, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
			h := New(cfg)
			defer h.Close()
			// A closed database makes normal quota admission fail closed.
			h.Limits = limits.New(db, time.UTC, time.Now)
			r := bodyRequest(t.Context(), "user", "interactive", tc.body)
			r.ContentLength = tc.length
			w := httptest.NewRecorder()
			h.forward(w, r, config.ProtocolOpenAI)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			assertBodySlotsReleased(t, h)
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type errorReader struct{ err error }

func (b errorReader) Read([]byte) (int, error) { return 0, b.err }

func TestRejectedUploadsReturnWithoutReadingFromNetwork(t *testing.T) {
	one := 1
	cfg := testConfig()
	cfg.Limits.MaxBodyReaders = 1
	cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}, MaxConcurrent: &one}}
	h, router := configuredForGroups(t, cfg, []string{"staff"})
	for _, framing := range []string{"Content-Length: 1000\r\n", "Transfer-Encoding: chunked\r\n"} {
		t.Run(strings.TrimSpace(framing), func(t *testing.T) {
			conn, err := net.Dial("tcp", strings.TrimPrefix(router.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			fmt.Fprintf(conn, "POST /v1/messages HTTP/1.1\r\nHost: x\r\n%s\r\n", framing)
			eventually(t, func() bool {
				h.bodies.mu.Lock()
				defer h.bodies.mu.Unlock()
				return h.bodies.active == 1
			})
			// No body bytes at all: rejection cannot depend on reading any.
			second, err := net.Dial("tcp", strings.TrimPrefix(router.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			second.SetDeadline(time.Now().Add(time.Second))
			fmt.Fprintf(second, "POST /v1/messages HTTP/1.1\r\nHost: x\r\n%s\r\n", framing)
			resp, err := http.ReadResponse(bufio.NewReader(second), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 429 || !resp.Close {
				t.Fatalf("status=%d close=%t", resp.StatusCode, resp.Close)
			}
			conn.Close()
			eventually(t, func() bool {
				h.bodies.mu.Lock()
				defer h.bodies.mu.Unlock()
				return h.bodies.active == 0
			})
		})
	}
	// Content-Length is checked even while the global reader slot is full.
	slot, _ := h.bodies.acquire("other", nil)
	defer slot.release()
	conn, err := net.Dial("tcp", strings.TrimPrefix(router.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	fmt.Fprintf(conn, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", MaxBody+1)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("oversized status=%d", resp.StatusCode)
	}
}

func TestBodySlotsReleasedBeforeBackendCompletes(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			close(entered)
			<-release
		}
		io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	h, router := setup(t, backend.URL)
	done := make(chan struct{})
	go func() { defer close(done); send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model"}`, nil) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("backend not reached")
	}
	assertBodySlotsReleased(t, h)
	unblock()
	<-done
}

func TestBodyRejectionsDoNotRead(t *testing.T) {
	cfg := testConfig()
	cfg.Limits = config.Limits{MaxBodyBytes: 16, MaxBodyReaders: 1, MaxBodyReadersPerUser: 1}
	h := New(cfg)
	defer h.Close()
	slot, _ := h.bodies.acquire("user", nil)
	defer slot.release()
	for _, tc := range []struct {
		user, protocol string
		length         int64
		status         int
	}{
		{"user", config.ProtocolOpenAI, -1, 429},
		{"other", config.ProtocolAnthropic, -1, 503},
		{"user", config.ProtocolAnthropic, 17, 413},
	} {
		w := httptest.NewRecorder()
		b := &stalledUpload{remaining: 100, ctx: t.Context(), ready: make(chan struct{})}
		r := bodyRequest(t.Context(), tc.user, "interactive", b)
		r.ContentLength = tc.length
		h.forward(w, r, tc.protocol)
		if w.Code != tc.status || b.read.Load() != 0 {
			t.Fatalf("status=%d want=%d bytes=%d", w.Code, tc.status, b.read.Load())
		}
	}
	slot.release()
	// Unknown-length bodies use the configured cap too.
	w := httptest.NewRecorder()
	h.forward(w, bodyRequest(t.Context(), "other", "interactive", io.NopCloser(strings.NewReader(strings.Repeat("x", 17)))), config.ProtocolOpenAI)
	if w.Code != 413 {
		t.Fatalf("configured streaming cap: %d", w.Code)
	}
	assertBodySlotsReleased(t, h)
}

func TestBodyAdmissionUsesEffectiveClassAndOverride(t *testing.T) {
	interactive, batch := 4, 1
	cfg := testConfig()
	cfg.Groups = map[string]config.Group{"staff": {MaxConcurrent: &interactive, MaxConcurrentBatch: &batch}}
	h := New(cfg)
	defer h.Close()
	slot, _ := h.bodies.acquire("user", nil)
	defer slot.release()
	for _, tc := range []struct {
		class     string
		overrides map[string]*int64
		status    int
	}{
		{"interactive", nil, 400}, // the second reader fits; malformed JSON
		{"batch", nil, 429},
		{"interactive", map[string]*int64{"max_concurrent": new(int64(1))}, 429},
	} {
		w := httptest.NewRecorder()
		r := bodyRequest(t.Context(), "user", tc.class, io.NopCloser(strings.NewReader("{")))
		p, _ := auth.FromContext(r.Context())
		p.Overrides = tc.overrides
		r = r.WithContext(auth.NewContext(r.Context(), p))
		h.forward(w, r, config.ProtocolOpenAI)
		if w.Code != tc.status {
			t.Fatalf("%s: %d want %d", tc.class, w.Code, tc.status)
		}
	}
	slot.release()
	assertBodySlotsReleased(t, h)
}
