package auth

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestCachedProtectedEndUserIsRefused(t *testing.T) {
	now := time.Now()
	a := &Authenticator{
		now: func() time.Time { return now }, listening: true,
		Forwarders: config.Forwarders{UnknownUser: config.UnknownForwarderPolicy, HeaderMap: map[string]string{"X-Olr-End-User": "username"}},
		endUsers: map[string]endUserEntry{
			endUserKey("username", "admin"): {principal: Principal{UserID: "root", ProtectedAdmin: true}, until: now.Add(time.Minute)},
		},
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-OLR-End-User", "admin")
	if _, err := a.forward(r, Principal{UserID: "forwarder", TrustedForwarder: true}); !errors.Is(err, errProtectedEndUser) {
		t.Fatal("cached protected identity was not refused", err)
	}
}
