package limits

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCalendarQuotaDeclinesSDKRetriesWithoutChangingReset(t *testing.T) {
	reset := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, limit := range []string{"tokens_per_day", "tokens_per_month", "max_concurrent", "max_concurrent_batch", "rpm", "tpm"} {
			t.Run(protocol+"/"+limit, func(t *testing.T) {
				w := httptest.NewRecorder()
				WriteError(w, protocol, &Rejection{Limit: limit, ResetAt: reset, RetryAfter: 86400})
				want := ""
				if limit == "tokens_per_day" || limit == "tokens_per_month" {
					want = "false"
				}
				if w.Code != 429 || w.Header().Get("X-Should-Retry") != want || w.Header().Get("Retry-After") != "86400" {
					t.Fatal("quota retry contract changed", w.Code, w.Header())
				}
				var body struct {
					Type  string
					Error struct {
						Limit   string
						ResetAt time.Time `json:"reset_at"`
					}
				}
				if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Limit != limit || !body.Error.ResetAt.Equal(reset) {
					t.Fatal("quota reset body changed")
				}
				if protocol == "anthropic" && body.Type != "error" {
					t.Fatal("Anthropic error envelope changed")
				}
			})
		}
	}
	w := httptest.NewRecorder()
	WriteError(w, "anthropic", ErrUnavailable)
	if w.Code != 503 || w.Header().Get("X-Should-Retry") != "" || w.Header().Get("Retry-After") != "1" {
		t.Fatal("transient failures became non-retryable")
	}
}
