package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/activity"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/backendlimits"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/metrics"
	"github.com/arm2arm/open-llm-router/internal/reqid"
	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/arm2arm/open-llm-router/internal/usage"
	"go.opentelemetry.io/otel/trace"
)

type accounting struct {
	writer       *statusWriter
	sink         usage.Recorder
	record       usage.Record
	json         *usage.JSON
	stream       *usage.Stream
	aborted      bool
	lease        *limits.Lease
	backendLease *backendlimits.Lease
	began        time.Time
	observer     *metrics.Observer
	span         trace.Span
	selection    time.Duration
	capture      *requesttrace.Capture
	live         *live
}

// live publishes metadata-only progress of one request to its user's portal.
type live struct {
	broker *activity.Broker
	user   string
	id     string
	began  time.Time
	model  *string
	last   time.Time
	bytes  int64
	writes int
}

func (l *live) event(kind string) activity.Event {
	return activity.Event{Type: kind, RequestID: l.id, Model: *l.model, ElapsedMS: time.Since(l.began).Milliseconds(), Bytes: l.bytes, Writes: l.writes}
}

// write runs on the forwarding path: a clock read and, at most every 500 ms
// and only while the user has the page open, a nonblocking publish.
func (l *live) write(n int) {
	l.bytes += int64(n)
	l.writes++
	now := time.Now()
	if (l.writes == 1 || now.Sub(l.last) >= activity.ProgressInterval) && l.broker.Watching(l.user) {
		l.last = now
		l.broker.Publish(l.user, l.event("progress"))
	}
}

func (h *Handler) account(w http.ResponseWriter, r *http.Request, protocol string) *accounting {
	a := &accounting{writer: &statusWriter{ResponseWriter: w}, record: usage.Record{StartedAt: time.Now(), Protocol: protocol}}
	a.began = a.record.StartedAt
	a.record.Operation = strings.TrimPrefix(r.URL.Path, "/v1/")
	a.record.RequestID = reqid.FromContext(r.Context())
	if a.record.RequestID == "" {
		a.record.RequestID = reqid.New()
	}
	if h.Limits != nil {
		a.record.StartedAt = h.Limits.Now()
	}
	if p, ok := auth.FromContext(r.Context()); ok && r.URL.Path != "/v1/messages/count_tokens" {
		a.sink = h.Usage
		a.record.UserID, a.record.KeyID, a.record.ForwardedBy = p.UserID, p.KeyID, p.ForwardedBy
		a.capture = h.Traces.Begin(p.UserID, strings.TrimPrefix(r.URL.Path, "/v1/"), a.began)
		a.writer.capture = a.capture
		a.capture.Session(r.Header.Get("X-Session-Id"))
		if h.Activity != nil {
			a.live = &live{broker: h.Activity, user: p.UserID, id: a.record.RequestID, began: a.began, model: &a.record.Model}
			a.writer.live = a.live
			h.Activity.Publish(p.UserID, activity.Event{Type: "started", RequestID: a.record.RequestID, Time: a.record.StartedAt, Operation: a.record.Operation, Protocol: protocol})
		}
	}
	return a
}

func (a *accounting) firstByte() {
	if a.record.TTFBMS == nil {
		ttfb := time.Since(a.began).Milliseconds()
		a.record.TTFBMS = &ttfb
	}
}

func (a *accounting) observe(contentType string, hideUsage bool, emit func([]byte) error) {
	contentType = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	a.capture.ResponseJSON(contentType == "application/json")
	if contentType == "text/event-stream" {
		a.record.IsStream = true
		a.stream = usage.NewStream(a.record.Protocol, hideUsage, emit)
	} else if contentType == "application/json" {
		a.json = &usage.JSON{Protocol: a.record.Protocol}
	}
}

func (a *accounting) finish(r *http.Request, panicked bool) {
	a.record.LatencyMS = time.Since(a.began).Milliseconds()
	status := a.writer.status
	if status == 0 {
		status = http.StatusOK
	}
	a.record.Status = strconv.Itoa(status)
	var totals usage.Totals
	if a.json != nil {
		totals = a.json.Totals
	}
	if a.stream != nil {
		totals = a.stream.Totals
		if status >= 200 && status < 300 {
			if totals.StreamError {
				a.record.Status = "error"
			} else if !totals.Complete {
				a.aborted = true
			}
		}
	}
	if a.aborted || panicked || r.Context().Err() != nil {
		a.record.Status = "aborted"
	}
	a.record.InputTokens, a.record.OutputTokens = totals.InputTokens, totals.Output()
	input, output := max(0, totals.InputTokens), max(0, totals.Output())
	backendTokens := min(input, 1<<63-1-output) + output
	if err := a.backendLease.Complete(backendTokens); err != nil {
		slog.Error("backend budget completion failed")
	}
	a.capture.Finish(a.record)
	if a.live != nil {
		r := a.record
		a.live.broker.Publish(a.live.user, activity.Event{Type: "finished", RequestID: r.RequestID, Time: r.StartedAt, Model: r.Model, Operation: r.Operation, Protocol: r.Protocol,
			Stream: r.IsStream, Status: r.Status, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, LatencyMS: r.LatencyMS, TTFBMS: r.TTFBMS, Bytes: a.live.bytes, Writes: a.live.writes})
	}
	if details := reqid.DetailsFromContext(r.Context()); details != nil {
		details.Record = a.record
	}
	if a.observer != nil {
		operation := "chat"
		switch r.URL.Path {
		case "/v1/completions":
			operation = "text_completion"
		case "/v1/embeddings":
			operation = "embeddings"
		case "/v1/messages/count_tokens":
			operation = "count_tokens"
		}
		a.observer.Finish(r.Context(), a.span, a.record, a.selection, operation)
	}
	if a.lease != nil {
		if r.URL.Path == "/v1/messages/count_tokens" {
			a.lease.Release()
		} else {
			a.lease.Complete(a.record)
		}
	}
	if a.sink != nil {
		a.sink.Record(a.record)
	}
}

// Unwrap keeps deadlines and flushing available through ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status    int
	capture   *requesttrace.Capture
	traceType uint8 // 0 unknown, 1 JSON, 2 SSE, 3 unsupported
	live      *live
}

func (w *statusWriter) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	if w.live != nil && n > 0 {
		w.live.write(n)
	}
	if w.capture != nil && n > 0 {
		if w.traceType == 0 {
			value, _, _ := strings.Cut(w.Header().Get("Content-Type"), ";")
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "application/json":
				w.traceType = 1
			case "text/event-stream":
				w.traceType = 2
			default:
				w.traceType = 3
			}
			w.capture.ResponseJSON(w.traceType == 1)
		}
		if w.traceType == 1 || w.traceType == 2 {
			w.capture.Write(p[:n])
		}
	}
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
