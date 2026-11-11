// Package usage observes protocol metadata and persists it without retaining
// prompts, completions, headers, or raw response events.
package usage

import "time"

// Record is one authenticated inference request. Model is the configured,
// canonical name (empty for an invalid request); Replica is the selected origin.
// TTFBMS is nil if no response body byte arrived. All durations start when the
// proxy receives the request, before reading its body.
type Record struct {
	StartedAt time.Time
	KeyID     string
	UserID    string // the user the call is attributed to
	// ForwardedBy is the key owner when a trusted forwarder made the call
	// for UserID; empty otherwise.
	ForwardedBy  string
	Model        string
	Replica      string
	Protocol     string
	Status       string
	InputTokens  int64
	OutputTokens int64
	LatencyMS    int64
	TTFBMS       *int64
	IsStream     bool
}

// TokensPerSecond derives the generation rate from the stored measurements.
// It is meaningful for streams; a non-stream response may arrive all at once.
func (r Record) TokensPerSecond() float64 {
	if r.TTFBMS == nil || r.LatencyMS <= *r.TTFBMS {
		return 0
	}
	return float64(r.OutputTokens) * 1000 / float64(r.LatencyMS-*r.TTFBMS)
}

// Recorder accepts metadata without waiting for storage.
type Recorder interface {
	Record(Record)
}

// Totals contains only numeric usage and stream state. ContentEvents is a
// lower-bound fallback, not a tokenizer: one event may contain several tokens.
type Totals struct {
	InputTokens, OutputTokens int64
	HasInput, HasOutput       bool
	ContentEvents             int64
	Complete, StreamError     bool
}

func (t Totals) Output() int64 {
	if t.HasOutput {
		return t.OutputTokens
	}
	return t.ContentEvents
}
