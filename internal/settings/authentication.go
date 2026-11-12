package settings

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/arm2arm/open-llm-router/internal/config"
)

// Authentication is the managed SSO connection. Disabled means local login
// only; the other values are kept so SSO can be re-enabled unchanged.
type Authentication struct {
	Enabled            bool           `json:"enabled"`
	DefaultSignIn      string         `json:"default_sign_in"` // keycloak (default) or local
	Issuer             string         `json:"issuer"`
	BackchannelURL     string         `json:"backchannel_url"`
	PortalClientID     string         `json:"portal_client_id"`
	PortalClientSecret SecretRef      `json:"portal_client_secret"`
	Audience           string         `json:"audience"`
	GroupsClaim        string         `json:"groups_claim"`
	GroupNames         string         `json:"group_names"` // leaf (default) or path
	UsernameClaim      string         `json:"username_claim"`
	RequiredClaim      *RequiredClaim `json:"required_claim"`
	SyncClientID       string         `json:"sync_client_id"`
	SyncClientSecret   SecretRef      `json:"sync_client_secret"`
	SyncIntervalS      int            `json:"sync_interval_s"`
}

type RequiredClaim struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SecretRef points at a secret without containing it: either the setting's
// own environment variable, or a stored, encrypted value by id. An empty
// reference means no secret. Other environment variables cannot be named, so
// an editor cannot redirect a database or bootstrap secret to an IdP.
type SecretRef struct {
	Env    string `json:"env,omitempty"`
	Stored string `json:"stored,omitempty"`
}

func (r SecretRef) empty() bool { return r.Env == "" && r.Stored == "" }

// secretField describes one secret setting of the domain.
type secretField struct {
	name string // setting name, also the stored secret's name suffix
	env  string // the only environment variable it may reference
	ref  func(*Authentication) *SecretRef
}

var authenticationSecrets = []secretField{
	{"portal_client_secret", config.EnvOIDCClientSecret, func(a *Authentication) *SecretRef { return &a.PortalClientSecret }},
	{"sync_client_secret", config.EnvOIDCSyncSecret, func(a *Authentication) *SecretRef { return &a.SyncClientSecret }},
}

func secretFieldNamed(name string) (secretField, bool) {
	for _, field := range authenticationSecrets {
		if field.name == name {
			return field, true
		}
	}
	return secretField{}, false
}

// decodeAuthentication reads a document strictly and fills defaults. Error
// messages name fields only; JSON errors never echo submitted values.
func decodeAuthentication(data []byte) (Authentication, error) {
	var doc Authentication
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		var field *json.UnmarshalTypeError
		switch {
		case errors.As(err, &field) && field.Field != "":
			return doc, &ValidationError{[]string{field.Field + ": has the wrong type"}}
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return doc, &ValidationError{[]string{strings.TrimPrefix(err.Error(), "json: ") + " in the authentication document"}}
		default:
			return doc, &ValidationError{[]string{"the document must be one JSON object"}}
		}
	}
	if decoder.More() {
		return doc, &ValidationError{[]string{"the document must be one JSON object"}}
	}
	if doc.GroupsClaim == "" {
		doc.GroupsClaim = "groups"
	}
	if doc.UsernameClaim == "" {
		doc.UsernameClaim = "preferred_username"
	}
	if doc.SyncClientID == "" {
		doc.SyncClientID = "olr-sync"
	}
	if doc.SyncIntervalS == 0 {
		doc.SyncIntervalS = 900
	}
	if doc.DefaultSignIn == "" {
		doc.DefaultSignIn = "keycloak"
	}
	if doc.GroupNames == "" {
		doc.GroupNames = "leaf"
	}
	return doc, nil
}

func plainText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func issuerURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// problems checks the document's own values; it does not resolve secrets.
func (a Authentication) problems() []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	for _, text := range [][2]string{{"issuer", a.Issuer}, {"backchannel_url", a.BackchannelURL}, {"portal_client_id", a.PortalClientID},
		{"audience", a.Audience}, {"groups_claim", a.GroupsClaim}, {"username_claim", a.UsernameClaim}, {"sync_client_id", a.SyncClientID}} {
		if !plainText(text[1], 2048) {
			add("%s: use at most 2048 bytes of text without control characters", text[0])
		}
	}
	if a.RequiredClaim != nil && (a.RequiredClaim.Name == "" || a.RequiredClaim.Value == "" ||
		!plainText(a.RequiredClaim.Name, 256) || !plainText(a.RequiredClaim.Value, 2048)) {
		add("required_claim: set both name and value, or use null")
	}
	if a.DefaultSignIn != "keycloak" && a.DefaultSignIn != "local" {
		add("default_sign_in: must be keycloak or local")
	}
	if a.GroupNames != "leaf" && a.GroupNames != "path" {
		add("group_names: must be leaf or path")
	}
	if a.SyncIntervalS < 60 || a.SyncIntervalS > 86400 {
		add("sync_interval_s: must be between 60 and 86400")
	}
	for _, field := range authenticationSecrets {
		ref := *field.ref(&a)
		switch {
		case ref.Env != "" && ref.Stored != "":
			add("%s: reference either the environment or a stored secret, not both", field.name)
		case ref.Env != "" && ref.Env != field.env:
			add("%s: the only environment reference allowed is %s", field.name, field.env)
		case ref.Stored != "" && !validUUID(ref.Stored):
			add("%s: stored must be a secret id", field.name)
		}
	}
	if !a.Enabled {
		return p
	}
	if !issuerURL(a.Issuer) {
		add("issuer: must be an http or https URL without credentials, query or fragment")
	}
	if a.BackchannelURL != "" && !issuerURL(a.BackchannelURL) {
		add("backchannel_url: must be empty or an http or https URL without credentials, query or fragment")
	}
	if a.PortalClientID == "" {
		add("portal_client_id: must be set")
	}
	return p
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// authenticationFromFile builds the import baseline from the effective file
// and environment configuration. Client secrets set in the environment become
// references to those variables, so importing changes no behavior.
func authenticationFromFile(file *config.Config) Authentication {
	doc := Authentication{DefaultSignIn: "keycloak", GroupNames: "leaf", GroupsClaim: "groups", UsernameClaim: "preferred_username", SyncClientID: "olr-sync", SyncIntervalS: 900}
	if o := file.OIDC; o != nil {
		doc.Enabled = true
		if o.DefaultSignIn != "" {
			doc.DefaultSignIn = o.DefaultSignIn
		}
		if o.GroupNames != "" {
			doc.GroupNames = o.GroupNames
		}
		doc.Issuer, doc.BackchannelURL, doc.PortalClientID, doc.Audience = o.Issuer, o.BackchannelURL, o.PortalClientID, o.Audience
		doc.GroupsClaim, doc.UsernameClaim, doc.SyncClientID, doc.SyncIntervalS = o.GroupsClaim, o.UsernameClaim, o.SyncClientID, o.SyncIntervalS
		if o.RequiredClaim != nil {
			doc.RequiredClaim = &RequiredClaim{Name: o.RequiredClaim.Name, Value: o.RequiredClaim.Value}
		}
	}
	if file.AuthSources["portal_client_secret"] == config.SourceEnvironment && file.OIDCClientSecret != "" {
		doc.PortalClientSecret.Env = config.EnvOIDCClientSecret
	}
	if file.AuthSources["sync_client_secret"] == config.SourceEnvironment && file.OIDCSyncSecret != "" {
		doc.SyncClientSecret.Env = config.EnvOIDCSyncSecret
	}
	return doc
}

// values returns the non-secret settings by name, in comparable form.
func (a Authentication) values() map[string]string {
	claim := ""
	if a.RequiredClaim != nil {
		claim = a.RequiredClaim.Name + "=" + a.RequiredClaim.Value
	}
	return map[string]string{
		"enabled": fmt.Sprint(a.Enabled), "default_sign_in": a.DefaultSignIn, "issuer": a.Issuer, "backchannel_url": a.BackchannelURL,
		"portal_client_id": a.PortalClientID, "audience": a.Audience, "groups_claim": a.GroupsClaim, "group_names": a.GroupNames,
		"username_claim": a.UsernameClaim, "required_claim": claim, "sync_client_id": a.SyncClientID,
		"sync_interval_s": fmt.Sprint(a.SyncIntervalS),
	}
}

// resolved holds the plaintext client secrets of an applied revision.
type resolved struct{ portal, sync string }

// apply builds the runtime configuration for a document and its secrets.
func (a Authentication) apply(base *config.Config, secrets resolved) (*config.Config, error) {
	var oidc *config.OIDC
	if a.Enabled {
		oidc = &config.OIDC{Issuer: a.Issuer, BackchannelURL: a.BackchannelURL, PortalClientID: a.PortalClientID, DefaultSignIn: a.DefaultSignIn,
			Audience: a.Audience, GroupsClaim: a.GroupsClaim, GroupNames: a.GroupNames, UsernameClaim: a.UsernameClaim,
			SyncClientID: a.SyncClientID, SyncIntervalS: a.SyncIntervalS}
		if a.RequiredClaim != nil {
			oidc.RequiredClaim = &config.RequiredClaim{Name: a.RequiredClaim.Name, Value: a.RequiredClaim.Value}
		}
	} else {
		secrets = resolved{}
	}
	cfg, err := config.WithAuthentication(base, oidc, secrets.portal, secrets.sync)
	if err != nil {
		// config problems name YAML keys and never contain values.
		var problems config.Problems
		if errors.As(err, &problems) {
			return nil, &ValidationError{problems}
		}
		return nil, &ValidationError{[]string{"the settings do not form a valid configuration"}}
	}
	return cfg, nil
}

// Conflict is a file or environment value that differs from the managed one.
// It names the setting and where it is set, never either value.
type Conflict struct {
	Setting string `json:"setting"`
	Source  string `json:"source"` // file or environment
	Where   string `json:"where"`  // e.g. oidc.issuer or OIDC_ISSUER
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s: %s sets a different value; remove %s so managed settings are the only source", c.Setting, c.Where, c.Where)
}

// conflicts compares file/environment values with a managed document.
// Secrets are compared in constant time; secret references to the setting's
// own environment variable are uses, not conflicts.
func conflicts(file *config.Config, doc Authentication, secrets resolved, secretsKnown bool) []Conflict {
	fileDoc := authenticationFromFile(file)
	fileValues, managed := fileDoc.values(), doc.values()
	var found []Conflict
	for _, setting := range config.AuthSettings {
		source := file.AuthSources[setting.Name]
		if source != config.SourceFile && source != config.SourceEnvironment {
			continue
		}
		where := setting.Env
		if source == config.SourceFile {
			where = "oidc"
			if setting.YAML != "" {
				where += "." + setting.YAML
			}
		}
		differs := false
		if field, ok := secretFieldNamed(setting.Name); ok {
			ref := *field.ref(&doc)
			fileSecret, managedSecret := file.OIDCClientSecret, secrets.portal
			if field.name == "sync_client_secret" {
				fileSecret, managedSecret = file.OIDCSyncSecret, secrets.sync
			}
			switch {
			case ref.Env == field.env:
			case ref.Stored != "" && secretsKnown:
				differs = subtle.ConstantTimeCompare([]byte(fileSecret), []byte(managedSecret)) != 1
			default:
				differs = fileSecret != "" || ref.Stored != ""
			}
		} else {
			differs = fileValues[setting.Name] != managed[setting.Name]
		}
		if differs {
			found = append(found, Conflict{Setting: setting.Name, Source: source, Where: where})
		}
	}
	return found
}
