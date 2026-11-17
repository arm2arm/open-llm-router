package server

import (
	"net/http"
	"strings"
)

const corsMethods = "GET, HEAD, POST, OPTIONS"

// This wrapper deliberately sits outside API authentication. Browsers send
// preflight without a key; all actual requests still pass through authentication.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	if len(s.cors.AllowedOrigins) == 0 {
		return next
	}
	origins := make(map[string]bool, len(s.cors.AllowedOrigins))
	for _, origin := range s.cors.AllowedOrigins {
		origins[origin] = true
	}
	headers := []string{"Authorization", "Content-Type", "Accept", "X-Request-Id", "X-Api-Key", "Anthropic-Version", "Anthropic-Beta"}
	headers = append(headers, s.cors.ExtraAllowedHeaders...)
	allowedHeaders := make(map[string]bool, len(headers))
	for _, header := range headers {
		allowedHeaders[strings.ToLower(header)] = true
	}
	headerList := strings.Join(headers, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" && !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Origin")
		originValues := r.Header.Values("Origin")
		if len(originValues) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if len(originValues) != 1 || !origins[originValues[0]] {
			http.Error(w, "browser origin is not allowed", http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", originValues[0])
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
		methods := r.Header.Values("Access-Control-Request-Method")
		if r.Method != http.MethodOptions || len(methods) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Access-Control-Request-Method")
		w.Header().Add("Vary", "Access-Control-Request-Headers")
		if len(methods) != 1 || !corsMethodAllowed(methods[0]) {
			http.Error(w, "preflight method is not allowed", http.StatusForbidden)
			return
		}
		for _, value := range r.Header.Values("Access-Control-Request-Headers") {
			for _, header := range strings.Split(value, ",") {
				if !allowedHeaders[strings.ToLower(strings.TrimSpace(header))] {
					http.Error(w, "preflight header is not allowed", http.StatusForbidden)
					return
				}
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", corsMethods)
		w.Header().Set("Access-Control-Allow-Headers", headerList)
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
	})
}

func corsMethodAllowed(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions:
		return true
	default:
		return false
	}
}
