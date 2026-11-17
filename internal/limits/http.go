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
		if (rejection.Limit == "tokens_per_day" || rejection.Limit == "tokens_per_month") && rejection.Reason != ReasonInFlightReservations {
			// Anthropic's default SDK retries 429s, potentially sleeping for
			// the calendar reset. Return the exhausted budget to its caller.
			w.Header().Set("X-Should-Retry", "false")
		}
		message := fmt.Sprintf("%s exceeded; retry at %s", rejection.Limit, rejection.ResetAt.Format(time.RFC3339))
		details := map[string]any{"limit": rejection.Limit, "reset_at": rejection.ResetAt}
		switch rejection.Reason {
		case ReasonInFlightReservations:
			message = fmt.Sprintf("%s temporarily reserved by in-flight requests; %d tokens remain before reservations; retry shortly", rejection.Limit, rejection.Remaining)
		case ReasonRequestExceedsRemaining:
			message = fmt.Sprintf("request token reservation exceeds the %d tokens remaining in %s; reduce the request or output limit, or wait until %s", rejection.Remaining, rejection.Limit, rejection.ResetAt.Format(time.RFC3339))
		}
		if rejection.Reason != "" {
			details["reason"], details["remaining_tokens"] = rejection.Reason, rejection.Remaining
		}
		apierr.WriteDetails(w, protocol, http.StatusTooManyRequests, "rate_limit_error", message, details)
		return
	}
	w.Header().Set("Retry-After", "1")
	apierr.Write(w, protocol, http.StatusServiceUnavailable, "api_error", ErrUnavailable.Error())
}
