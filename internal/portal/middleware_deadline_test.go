package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOnlyLiveEventsAvoidPortalJSONDeadline(t *testing.T) {
	for _, path := range []string{"/portal/api/requests/events", "/portal/api/requests", "/portal/api/me", "/portal/api/requests/events/other"} {
		t.Run(path, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				deadline, ok := r.Context().Deadline()
				if path == "/portal/api/requests/events" {
					if ok {
						t.Fatal("live stream inherits JSON deadline")
					}
				} else if !ok || time.Until(deadline) > 10*time.Second || time.Until(deadline) < 9*time.Second {
					t.Fatal("JSON route lost its deadline", deadline, ok)
				}
				cancel()
				select {
				case <-r.Context().Done():
				default:
					t.Fatal("parent cancellation lost")
				}
			})
			h := Handler{mux: mux}
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil).WithContext(parent))
		})
	}
}
