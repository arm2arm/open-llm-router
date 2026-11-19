package auth

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProfileClaims(t *testing.T) {
	for _, tt := range []struct {
		name, document, claim string
		old, want             Profile
	}{
		{"standard", `{"name":"Dr. Ada Lovelace","given_name":"Ada","family_name":"Lovelace","institution":"Example Institute"}`, "", Profile{}, Profile{"Dr. Ada Lovelace", "Ada", "Lovelace", "Example Institute"}},
		{"name fallback", `{"given_name":"Ada","family_name":"Lovelace"}`, "", Profile{}, Profile{"Ada Lovelace", "Ada", "Lovelace", ""}},
		{"federation", `{"schacHomeOrganization":"example.edu"}`, "", Profile{}, Profile{Institution: "example.edu"}},
		{"multiple", `{"organization":["Institute A","Institute B"]}`, "", Profile{}, Profile{Institution: "Institute A; Institute B"}},
		{"custom", `{"affiliation":"Research Institute","institution":"ignored"}`, "affiliation", Profile{}, Profile{Institution: "Research Institute"}},
		{"explicit mapping only", `{"institution":"ignored"}`, "affiliation", Profile{Institution: "Original"}, Profile{Institution: "Original"}},
		{"omitted", `{}`, "", Profile{"Original Name", "Original", "Name", "Original Institute"}, Profile{"Original Name", "Original", "Name", "Original Institute"}},
		{"malformed optional fields", `{"name":{},"given_name":[],"family_name":false,"institution":{"id":"org"}}`, "", Profile{FullName: "Original"}, Profile{FullName: "Original"}},
		{"null preserves", `{"name":null,"institution":null}`, "", Profile{FullName: "Original", Institution: "Institute"}, Profile{FullName: "Original", Institution: "Institute"}},
		{"explicit clearing", `{"name":"","given_name":"","family_name":"","institution":""}`, "", Profile{"Old Name", "Old", "Name", "Institute"}, Profile{}},
		{"updated fallback", `{"given_name":"New"}`, "", Profile{"Old Name", "Old", "Name", "Institute"}, Profile{"New Name", "New", "Name", "Institute"}},
		{"unicode and controls", `{"given_name":"  Zoë\n","family_name":"O’Connor\u0000","institution":"  Universität\tBeispiel  "}`, "", Profile{}, Profile{"Zoë O’Connor", "Zoë", "O’Connor", "Universität Beispiel"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var claims map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tt.document), &claims); err != nil {
				t.Fatal(err)
			}
			if got := ReadProfileClaims(claims, tt.claim).apply(tt.old); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestProfileValuesStayBounded(t *testing.T) {
	value, _ := json.Marshal(strings.Repeat("界", 1000))
	claims := ReadProfileClaims(map[string]json.RawMessage{"name": value, "institution": value}, "")
	profile := claims.apply(Profile{})
	if len(profile.FullName) > 1024 || len(profile.Institution) > 1024 || !utf8.ValidString(profile.FullName) || !utf8.ValidString(profile.Institution) {
		t.Fatal("profile exceeded storage bounds or split a Unicode character")
	}
}
