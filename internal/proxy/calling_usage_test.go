package proxy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
)

func TestCallingUsageKey(t *testing.T) {
	p := auth.Principal{
		KeyID: "key-id", KeyPrefix: "olr-prefix", KeyLabel: "overnight",
		KeyClass: "batch", KeyExpiresAt: time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	for _, tc := range []struct {
		name string
		p    auth.Principal
		want string
	}{
		{"batch", p, `{"id":"key-id","prefix":"olr-prefix","label":"overnight","class":"batch","expires":"2027-01-02T03:04:05Z"}`},
		{"legacy key", auth.Principal{KeyID: "legacy", KeyClass: "interactive"}, `{"id":"legacy","prefix":"","label":"","class":"interactive","expires":null}`},
		{"OIDC token", auth.Principal{UserID: "user", KeyClass: "interactive"}, "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(callingUsageKey(tc.p))
			if err != nil || string(data) != tc.want {
				t.Fatalf("calling key JSON = %s, err = %v; want %s", data, err, tc.want)
			}
		})
	}
}
