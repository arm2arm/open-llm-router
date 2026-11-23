package auth

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Profile is display-only identity-provider data. It never grants access or
// links accounts; issuer and subject remain the identity key.
type Profile struct {
	FullName    string `json:"full_name"`
	GivenName   string `json:"given_name"`
	FamilyName  string `json:"family_name"`
	Institution string `json:"institution"`
}

// ProfileClaims distinguishes an omitted attribute from an explicit empty one.
// Omitted or malformed optional claims do not erase previously supplied data.
type ProfileClaims struct {
	FullName, GivenName, FamilyName, Institution *string
}

// ReadProfileClaims reads verified ID-token claims, not arbitrary user input.
// Institution is not an OIDC standard claim. An explicit mapping selects one
// top-level claim; otherwise common institutional/federation names are tried.
func ReadProfileClaims(claims map[string]json.RawMessage, institutionClaim string) ProfileClaims {
	p := ProfileClaims{
		FullName:   profileText(claims["name"], false),
		GivenName:  profileText(claims["given_name"], false),
		FamilyName: profileText(claims["family_name"], false),
	}
	names := []string{"institution", "organization", "schacHomeOrganization"}
	if institutionClaim != "" {
		names = []string{institutionClaim}
	}
	for _, name := range names {
		if value := profileText(claims[name], true); value != nil {
			p.Institution = value
			break
		}
	}
	return p
}

func profileText(raw json.RawMessage, multiple bool) *string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		var values []string
		if !multiple || json.Unmarshal(raw, &values) != nil || len(values) > 8 {
			return nil
		}
		value = strings.Join(values, "; ")
	}
	value = cleanProfileText(value)
	return &value
}

func cleanProfileText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 1024 {
		value = value[:1024]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return strings.TrimSpace(value)
}

func (c ProfileClaims) apply(p Profile) Profile {
	for _, field := range []struct {
		source *string
		target *string
	}{{c.GivenName, &p.GivenName}, {c.FamilyName, &p.FamilyName}, {c.Institution, &p.Institution}, {c.FullName, &p.FullName}} {
		if field.source != nil {
			*field.target = cleanProfileText(*field.source)
		}
	}
	if c.FullName == nil && (c.GivenName != nil || c.FamilyName != nil) {
		p.FullName = cleanProfileText(p.GivenName + " " + p.FamilyName)
	}
	return p
}
