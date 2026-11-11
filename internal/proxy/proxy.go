// Package proxy forwards both API protocols without translating their payloads.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/apierr"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/metrics"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/arm2arm/open-llm-router/internal/reqid"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

const MaxBody = 32 << 20

var errHeaderTimeout = errors.New("upstream response headers timed out")
var errIdleTimeout = errors.New("upstream response stalled")

type Handler struct {
	cfg       *config.Config
	transport *http.Transport
	Pools     *pool.Manager
	Usage     usage.Recorder    // installed before serving; nil disables persistence
	Limits    *limits.Manager   // installed before serving
	Metrics   *metrics.Observer // installed before serving
	active    sync.WaitGroup
}

func New(cfg *config.Config) *Handler {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.MaxIdleConns, t.MaxIdleConnsPerHost, t.MaxConnsPerHost = 1024, 256, 0
	t.DisableCompression = true
	// A cancellable timer below applies each model's header timeout while
	// retaining a single shared transport and its connection pool.
	t.ResponseHeaderTimeout = 0
	return &Handler{cfg: cfg, transport: t, Pools: pool.New(cfg, t)}
}

// Close waits for handlers after the HTTP server stops, then closes idle
// connections. The usage writer must be drained afterwards.
func (h *Handler) Close() {
	h.active.Wait()
	h.transport.CloseIdleConnections()
}

func (h *Handler) Register(handle func(string, http.Handler)) {
	for path, protocol := range map[string]string{
		"/v1/chat/completions":      config.ProtocolOpenAI,
		"/v1/completions":           config.ProtocolOpenAI,
		"/v1/embeddings":            config.ProtocolOpenAI,
		"/v1/messages":              config.ProtocolAnthropic,
		"/v1/messages/count_tokens": config.ProtocolAnthropic,
	} {
		handle("POST "+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.forward(w, r, protocol) }))
	}
	handle("GET /v1/models", http.HandlerFunc(h.models))
	handle("GET /v1/me/usage", http.HandlerFunc(h.myUsage))
}

func (h *Handler) myUsage(w http.ResponseWriter, r *http.Request) {
	if h.Limits == nil {
		limits.WriteError(w, config.ProtocolOpenAI, limits.ErrUnavailable)
		return
	}
	principal, _ := auth.FromContext(r.Context())
	snapshot, err := h.Limits.Snapshot(r.Context(), principal.UserID, h.callerPolicy(r))
	if err != nil {
		limits.WriteError(w, config.ProtocolOpenAI, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}

func (h *Handler) callerPolicy(r *http.Request) policy.Policy {
	p, _ := auth.FromContext(r.Context())
	return policy.Resolve(h.cfg.Groups, p.Groups).WithOverrides(p.Overrides)
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	access := h.callerPolicy(r)
	names := slices.AppendSeq(slices.Collect(maps.Keys(h.cfg.Models)), maps.Keys(h.cfg.Aliases))
	slices.Sort(names)
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		target := name
		if model, ok := h.cfg.Aliases[name]; ok {
			target = model
		}
		if !access.AllowsModel(target) {
			continue
		}
		data = append(data, map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "open-llm-router"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, protocol string) {
	h.active.Add(1)
	defer h.active.Done()
	account := h.account(w, r, protocol)
	if h.Metrics != nil {
		ctx, span := h.Metrics.Start(r, protocol)
		r = r.WithContext(ctx)
		account.observer, account.span = h.Metrics, span
	}
	w = account.writer
	defer func() {
		p := recover()
		account.finish(r, p != nil)
		if p != nil {
			panic(p)
		}
	}()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apierr.Write(w, protocol, 413, "request_too_large", "request body exceeds 32 MiB")
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			w.Header().Set("Connection", "close")
			apierr.Write(w, protocol, 408, "invalid_request_error", "request body was not received in time")
		} else {
			apierr.Write(w, protocol, 400, "invalid_request_error", "request body is unreadable")
		}
		return
	}
	// The server bounds the body upload; a response may stream for much longer.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	meta, err := parseRequest(body)
	if errors.Is(err, errAmbiguousField) {
		apierr.Write(w, protocol, 400, "invalid_request_error", "model, stream, max_tokens and max_completion_tokens must each appear once, spelled in lower case")
		return
	}
	if err != nil || meta.Model == "" {
		apierr.Write(w, protocol, 400, "invalid_request_error", "a JSON object with a model and optional boolean stream is required")
		return
	}
	name := meta.Model
	if target, ok := h.cfg.Aliases[name]; ok {
		name = target
	}
	model, ok := h.cfg.Models[name]
	if !ok {
		apierr.Write(w, protocol, 404, "not_found_error", "model is unavailable for this protocol")
		return
	}
	account.record.Model = name
	access := h.callerPolicy(r)
	if !access.AllowsModel(name) {
		apierr.Write(w, protocol, 403, "permission_error", "access to this model is forbidden")
		return
	}
	if !slices.Contains(model.Protocols, protocol) {
		apierr.Write(w, protocol, 404, "not_found_error", "model is unavailable for this protocol")
		return
	}
	upstreamName := name
	if model.UpstreamModel != "" {
		upstreamName = model.UpstreamModel
	}
	if upstreamName != meta.Model {
		// RawMessage retains unknown values, including large integers, while
		// replacing only the model. Otherwise forward the original bytes.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		fields["model"], _ = json.Marshal(upstreamName)
		body, _ = json.Marshal(fields)
	}
	hideUsage := false
	if protocol == config.ProtocolOpenAI && meta.Stream && r.URL.Path != "/v1/embeddings" {
		var asked bool
		body, asked, err = usage.IncludeUsage(body)
		if err != nil {
			apierr.Write(w, protocol, 400, "invalid_request_error", "stream_options must contain an optional boolean include_usage")
			return
		}
		hideUsage = !asked
	}
	if h.Limits != nil {
		principal, _ := auth.FromContext(r.Context())
		admission := limits.Admission{User: principal.UserID, Class: principal.KeyClass}
		if access.StrictQuota && r.URL.Path != "/v1/messages/count_tokens" {
			// The request size bounds its input tokens: a token covers at
			// least one byte of text. Over-reserving lasts only while in flight.
			if meta.MaxTokens == 0 && r.URL.Path != "/v1/embeddings" {
				apierr.Write(w, protocol, 400, "invalid_request_error", "your group has a strict quota: set max_tokens")
				return
			}
			admission.Reserve = meta.MaxTokens + int64(len(body))
		}
		account.lease, err = h.Limits.AcquireFor(r.Context(), admission, access)
		if err != nil {
			var rejected *limits.Rejection
			if h.Metrics != nil && errors.As(err, &rejected) {
				h.Metrics.Reject(name, protocol, rejected.Limit)
			}
			limits.WriteError(w, protocol, err)
			return
		}
	}
	var previous *pool.Lease
	for attempt := 0; attempt < 2; attempt++ {
		if r.Context().Err() != nil {
			return
		}
		selectionStarted := time.Now()
		lease := h.Pools.Pools[name].Acquire(previous)
		account.selection += time.Since(selectionStarted)
		if lease == nil {
			w.Header().Set("Retry-After", "1")
			apierr.Write(w, protocol, 503, "overloaded_error", "no healthy replica with available capacity")
			return
		}
		account.record.Replica = lease.Replica.URL
		committed, err := h.attempt(w, r, body, model, lease, account, hideUsage)
		if err == nil || committed || r.Context().Err() != nil {
			return
		}
		previous = lease
	}
	apierr.Write(w, protocol, 502, "api_error", "backend connection failed before the response started")
}

// attempt reserves its lease until the response is finished or aborted. The
// first read happens before committing headers, allowing one safe failover.
func (h *Handler) attempt(w http.ResponseWriter, r *http.Request, body []byte, model config.Model, lease *pool.Lease, account *accounting, hideUsage bool) (committed bool, err error) {
	defer lease.Release()
	defer func() {
		if err != nil && !committed && r.Context().Err() == nil {
			lease.Failed()
		}
	}()
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	u, _ := url.Parse(lease.Replica.URL)
	u.Path, u.RawPath, u.RawQuery = r.URL.Path, r.URL.RawPath, r.URL.RawQuery
	u.ForceQuery = r.URL.ForceQuery
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header = cleanHeaders(r.Header)
	if h.Metrics != nil {
		metrics.Inject(ctx, req.Header)
	}
	req.Header.Del("Baggage")
	for _, key := range []string{"Authorization", "X-Api-Key", "Cookie", "Content-Length", "Host"} {
		req.Header.Del(key)
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-Request-Id", reqid.FromContext(r.Context()))
	if lease.Replica.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+lease.Replica.BearerToken)
	}
	// NewRequest supplies a new body for the one deliberate retry. RoundTrip
	// does not follow redirects; avoid transport retries of this POST too.
	req.GetBody = nil
	headerTimer := time.AfterFunc(time.Duration(model.TimeoutS)*time.Second, func() { cancel(errHeaderTimeout) })
	resp, err := h.transport.RoundTrip(req)
	headerTimer.Stop()
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	idleTimer := time.AfterFunc(time.Duration(model.IdleTimeoutS)*time.Second, func() { cancel(errIdleTimeout) })
	defer idleTimer.Stop()
	buf := make([]byte, 32<<10)
	n, readErr := resp.Body.Read(buf)
	if n == 0 && readErr != nil && readErr != io.EOF {
		return false, readErr
	}
	if n > 0 {
		account.firstByte()
	}
	for key, values := range cleanHeaders(resp.Header) {
		w.Header()[key] = values
	}
	// The router's origin also serves the portal: a backend must not be able
	// to set or overwrite cookies there.
	w.Header().Del("Set-Cookie")
	w.Header().Set("X-Request-Id", reqid.FromContext(r.Context()))
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	write := func(p []byte) error {
		_, err := w.Write(p)
		return err
	}
	account.observe(resp.Header.Get("Content-Type"), hideUsage, write)
	if account.stream != nil && hideUsage {
		// A filtered event changes the length, even if the backend supplied a
		// Content-Length for its SSE response.
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	for {
		_ = controller.SetWriteDeadline(time.Now().Add(time.Duration(model.IdleTimeoutS) * time.Second))
		if n > 0 {
			account.firstByte()
			var err error
			if account.stream != nil {
				err = account.stream.Write(buf[:n])
			} else {
				if account.json != nil {
					account.json.Write(buf[:n])
				}
				err = write(buf[:n])
			}
			if err != nil {
				account.aborted = true
				return true, err
			}
			if err := controller.Flush(); err != nil {
				account.aborted = true
				return true, err
			}
		}
		if readErr != nil {
			if account.stream != nil {
				if err := account.stream.Finish(); err != nil {
					account.aborted = true
					return true, err
				}
			}
			if readErr != io.EOF {
				// Terminate the HTTP response instead of reporting a truncated
				// stream as a successful, complete response.
				account.aborted = true
				panic(http.ErrAbortHandler)
			}
			return true, nil
		}
		idleTimer.Reset(time.Duration(model.IdleTimeoutS) * time.Second)
		// Also bound a blocked downstream write so slow/disconnected clients
		// cannot retain a backend slot indefinitely.
		n, readErr = resp.Body.Read(buf)
	}
}

func cleanHeaders(source http.Header) http.Header {
	h := source.Clone()
	for _, value := range source.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			h.Del(strings.TrimSpace(token))
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
	return h
}
