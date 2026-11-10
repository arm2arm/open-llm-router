// Package server holds the router's HTTP handler and runs it: routes, request
// IDs, the request log, health endpoints and graceful shutdown.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/arm2arm/open-llm-router/internal/reqid"
)

const (
	// readHeaderTimeout bounds how long a client may take to send its request
	// headers. The server sets no ReadTimeout and no WriteTimeout: both are
	// deadlines for a whole exchange, and responses stream for minutes.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes keep-alive connections that carry no request.
	idleTimeout = 2 * time.Minute
)

// Server is the router's HTTP server.
type Server struct {
	log   *slog.Logger
	mux   *http.ServeMux
	ready func() bool
}

// New returns a server that answers the health endpoints. Further routes are
// added with Handle.
func New(log *slog.Logger) *Server {
	s := &Server{log: log, mux: http.NewServeMux(), ready: func() bool { return true }}
	s.mux.HandleFunc("GET /healthz", status("ok"))
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			status("unavailable")(w, r)
			return
		}
		status("ready")(w, r)
	})
	return s
}

// SetReadiness installs the readiness probe before serving requests.
func (s *Server) SetReadiness(check func() bool) { s.ready = check }

func status(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"status\":%q}\n", text)
	}
}

// Handle registers a handler for a method-and-path pattern such as
// "POST /v1/chat/completions".
func (s *Server) Handle(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
}

// Handler returns the complete HTTP handler: the routes wrapped in what every
// request gets. Each request is given an ID (in the X-Request-Id response
// header and in its context, see package reqid) and one log line when it
// ends. The log line holds metadata only: no headers, no query string, no
// client address, and never a request or response body.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := reqid.New()
		w.Header().Set("X-Request-Id", id)
		rec := &recorder{ResponseWriter: w}

		defer func() {
			p := recover()
			if p != nil && p != http.ErrAbortHandler {
				s.log.Error("handler panicked", "request_id", id, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if rec.status == 0 {
					http.Error(rec, "internal server error", http.StatusInternalServerError)
					p = nil
				} else {
					// The response has started and cannot be turned into an
					// error any more. Abort the connection, so the client sees
					// a broken response and not a complete one.
					p = http.ErrAbortHandler
				}
			}
			status := rec.status
			if status == 0 && p == nil {
				status = http.StatusOK // what net/http sends when a handler writes nothing
			}
			probe := r.URL.Path == "/healthz" || r.URL.Path == "/readyz"
			if !probe || status != http.StatusOK {
				s.log.Info("request",
					"request_id", id,
					"method", r.Method,
					"path", r.URL.Path,
					"status", status,
					"bytes", rec.bytes,
					"duration_ms", time.Since(start).Milliseconds(),
				)
			}
			if p != nil {
				panic(p)
			}
		}()

		s.mux.ServeHTTP(rec, r.WithContext(reqid.NewContext(r.Context(), id)))
	})
}

// Run serves on ln until ctx is cancelled. It then stops accepting
// connections and gives the requests in flight the grace period to finish.
// If some are still running after that, their connections are closed and Run
// returns an error.
func (s *Server) Run(ctx context.Context, ln net.Listener, grace time.Duration) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "grace", grace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close()
		return fmt.Errorf("requests were still running after the grace period of %s and were cut off", grace)
	}
	return nil
}

// recorder notes the status and size of a response for the request log.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 && code >= 200 { // 1xx responses are not the final one
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Flush and Unwrap keep streaming working through the recorder: Flush for
// code that asks for an http.Flusher, Unwrap for http.ResponseController.
func (r *recorder) Flush() {
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
