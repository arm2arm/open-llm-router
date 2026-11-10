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
	"slices"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/apierr"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/arm2arm/open-llm-router/internal/reqid"
)

const MaxBody = 32 << 20

var errHeaderTimeout = errors.New("upstream response headers timed out")
var errIdleTimeout = errors.New("upstream response stalled")

type Handler struct {
	cfg       *config.Config
	transport *http.Transport
	Pools     *pool.Manager
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

func (h *Handler) Close() { h.transport.CloseIdleConnections() }

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
}

func (h *Handler) models(w http.ResponseWriter, _ *http.Request) {
	names := slices.AppendSeq(slices.Collect(maps.Keys(h.cfg.Models)), maps.Keys(h.cfg.Aliases))
	slices.Sort(names)
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "open-llm-router"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, protocol string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apierr.Write(w, protocol, 413, "request_too_large", "request body exceeds 32 MiB")
		} else {
			apierr.Write(w, protocol, 400, "invalid_request_error", "request body is unreadable")
		}
		return
	}
	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.Unmarshal(body, &meta) != nil || meta.Model == "" {
		apierr.Write(w, protocol, 400, "invalid_request_error", "a JSON object with a model and optional boolean stream is required")
		return
	}
	name := meta.Model
	if target, ok := h.cfg.Aliases[name]; ok {
		name = target
	}
	model, ok := h.cfg.Models[name]
	if !ok || !slices.Contains(model.Protocols, protocol) {
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
	var previous *pool.Lease
	for attempt := 0; attempt < 2; attempt++ {
		if r.Context().Err() != nil {
			return
		}
		lease := h.Pools.Pools[name].Acquire(previous)
		if lease == nil {
			w.Header().Set("Retry-After", "1")
			apierr.Write(w, protocol, 503, "overloaded_error", "no healthy replica with available capacity")
			return
		}
		committed, err := h.attempt(w, r, body, model, lease)
		if err == nil || committed || r.Context().Err() != nil {
			return
		}
		previous = lease
	}
	apierr.Write(w, protocol, 502, "api_error", "backend connection failed before the response started")
}

// attempt reserves its lease until the response is finished or aborted. The
// first read happens before committing headers, allowing one safe failover.
func (h *Handler) attempt(w http.ResponseWriter, r *http.Request, body []byte, model config.Model, lease *pool.Lease) (committed bool, err error) {
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
	for key, values := range cleanHeaders(resp.Header) {
		w.Header()[key] = values
	}
	w.Header().Set("X-Request-Id", reqid.FromContext(r.Context()))
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	controller := http.NewResponseController(w)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	for {
		_ = controller.SetWriteDeadline(time.Now().Add(time.Duration(model.IdleTimeoutS) * time.Second))
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return true, err
			}
			if err := controller.Flush(); err != nil {
				return true, err
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				// Terminate the HTTP response instead of reporting a truncated
				// stream as a successful, complete response.
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
