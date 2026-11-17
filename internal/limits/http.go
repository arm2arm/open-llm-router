package limits

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/arm2arm/open-llm-router/internal/apierr"
)

func WriteError(w http.ResponseWriter, protocol string, err error) {
	var rejection *Rejection
	if errors.As(err, &rejection) {
		w.Header().Set("Retry-After", strconv.Itoa(rejection.RetryAfter))
		if rejection.Limit == "tokens_per_day" || rejection.Limit == "tokens_per_month" {
			// Anthropic's default SDK retries 429s, potentially sleeping for
			// the calendar reset. Return the exhausted budget to its caller.
			w.Header().Set("X-Should-Retry", "false")
		}
		apierr.WriteDetails(w, protocol, http.StatusTooManyRequests, "rate_limit_error",
			fmt.Sprintf("%s exceeded; retry at %s", rejection.Limit, rejection.ResetAt.Format(time.RFC3339)),
			map[string]any{"limit": rejection.Limit, "reset_at": rejection.ResetAt})
		return
	}
	w.Header().Set("Retry-After", "1")
	apierr.Write(w, protocol, http.StatusServiceUnavailable, "api_error", ErrUnavailable.Error())
}
