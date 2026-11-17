package oidcclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestDiagnosticCausesNeverExposeProviderText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("https://name:private@provider.invalid: %w", context.DeadlineExceeded), "request timed out"},
		{&net.DNSError{Name: "private.provider.invalid", Err: "secret"}, "DNS lookup failed"},
		{errors.New("https://name:private@provider.invalid?secret=value response body"), "provider connection, response or discovery metadata invalid"},
	} {
		if got := DiagnosticCause(tc.err); got != tc.want {
			t.Fatalf("unexpected classified cause %q", got)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "private response body")
	}))
	defer server.Close()
	_, err := New(t.Context(), &config.OIDC{Issuer: server.URL})
	if err == nil || err.Error() != "OIDC discovery failed" || DiagnosticCause(err) != "identity provider returned HTTP 503" {
		t.Fatal("discovery status not classified safely", err)
	}
	if strings.Contains(DiagnosticCause(err), "private") || strings.Contains(DiagnosticCause(err), server.URL) {
		t.Fatal("provider details escaped")
	}
}
