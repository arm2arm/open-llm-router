package proxy

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

type accounting struct {
	writer  *statusWriter
	sink    usage.Recorder
	record  usage.Record
	json    *usage.JSON
	stream  *usage.Stream
	aborted bool
	lease   *limits.Lease
	began   time.Time
}

func (h *Handler) account(w http.ResponseWriter, r *http.Request, protocol string) *accounting {
	a := &accounting{writer: &statusWriter{ResponseWriter: w}, record: usage.Record{StartedAt: time.Now(), Protocol: protocol}}
	a.began = a.record.StartedAt
	if h.Limits != nil {
		a.record.StartedAt = h.Limits.Now()
	}
	if p, ok := auth.FromContext(r.Context()); ok && r.URL.Path != "/v1/messages/count_tokens" {
		a.sink = h.Usage
		a.record.UserID, a.record.KeyID = p.UserID, p.KeyID
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
	if contentType == "text/event-stream" {
		a.record.IsStream = true
		a.stream = usage.NewStream(a.record.Protocol, hideUsage, emit)
	} else if contentType == "application/json" {
		a.json = &usage.JSON{Protocol: a.record.Protocol}
	}
}

func (a *accounting) finish(r *http.Request, panicked bool) {
	if a.sink == nil && a.lease == nil {
		return
	}
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
	status int
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
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
