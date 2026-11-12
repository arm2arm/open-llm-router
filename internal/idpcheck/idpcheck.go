// Package idpcheck tests an SSO connection before it is used: discovery and
// logout metadata, portal client credentials, a service token's audience and
// the group-sync service account's permissions. Results carry statuses and
// fixed messages, never secrets, tokens or provider responses.
//
// Connections go only to the issuer (or its back-channel URL), never follow
// redirects and refuse link-local, metadata, multicast and unspecified
// addresses at dial time. Loopback and private networks are allowed because
// identity providers commonly run on internal networks.
package idpcheck

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/groupsync"
	"github.com/arm2arm/open-llm-router/internal/oidcclient"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2/clientcredentials"
)

// Statuses of a check.
const (
	Passed  = "passed"
	Failed  = "failed"
	Skipped = "skipped"
)

// Check names, in the order they run.
const (
	Discovery     = "discovery"
	Logout        = "logout_metadata"
	Portal        = "portal_credentials"
	TokenAudience = "service_token_audience"
	SyncAccess    = "sync_permissions"
	TestSignIn    = "test_sign_in"
)

type Result struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Input is a candidate connection. TestClient* optionally name a service
// client used only to obtain one token for the audience check; they are not
// stored.
type Input struct {
	OIDC             *config.OIDC
	PortalSecret     string
	SyncSecret       string
	TestClientID     string
	TestClientSecret string
}

var errRefused = errors.New("refused destination")

// metadata are cloud instance-metadata addresses outside link-local ranges.
var metadata = []netip.Addr{netip.MustParseAddr("fd00:ec2::254")}

// Allow refuses addresses an identity provider never legitimately uses.
func Allow(addr netip.Addr) error {
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() {
		return errRefused
	}
	for _, refused := range metadata {
		if addr == refused {
			return errRefused
		}
	}
	return nil
}

// Connect discovers the provider with the destination restrictions; the
// caller closes the client. Its error is one of the check messages.
func Connect(ctx context.Context, cfg *config.OIDC) (*oidcclient.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := oidcclient.NewWith(ctx, cfg, oidcclient.Options{Allow: Allow})
	switch {
	case err == nil:
		return client, nil
	case errors.Is(err, oidcclient.ErrIssuerMismatch):
		return nil, errors.New("the provider's discovery document names a different issuer; use its exact issuer URL")
	case strings.Contains(err.Error(), errRefused.Error()):
		return nil, errors.New("the issuer resolves to a refused address (link-local, metadata, multicast or unspecified)")
	default:
		return nil, errors.New("the issuer's OpenID configuration could not be read; check the issuer and back-channel URLs")
	}
}

// Run executes the connection checks in order. Later checks are skipped
// when discovery fails.
func Run(ctx context.Context, in Input) []Result {
	if in.OIDC == nil {
		return []Result{{Discovery, Skipped, "single sign-on is disabled in these settings"}}
	}
	idp, err := Connect(ctx, in.OIDC)
	if err != nil {
		results := []Result{{Discovery, Failed, err.Error()}}
		for _, name := range []string{Logout, Portal, TokenAudience, SyncAccess} {
			results = append(results, Result{name, Skipped, "needs a successful discovery"})
		}
		return results
	}
	defer idp.Close()
	results := []Result{{Discovery, Passed, "the issuer's OpenID configuration was read and names this issuer"}}
	var metadata struct {
		Logout     string `json:"end_session_endpoint"`
		Introspect string `json:"introspection_endpoint"`
	}
	_ = idp.Provider.Claims(&metadata)
	if httpURL(metadata.Logout) {
		results = append(results, Result{Logout, Passed, "the provider publishes an end-session endpoint for sign-out"})
	} else {
		results = append(results, Result{Logout, Failed, "the provider publishes no valid end_session_endpoint; sign-out could not end the provider session"})
	}
	results = append(results, portalCredentials(ctx, idp, in, metadata.Introspect))
	results = append(results, tokenAudience(ctx, idp, in))
	results = append(results, syncPermissions(ctx, idp, in))
	return results
}

func httpURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// portalCredentials authenticates the portal client against the token
// introspection endpoint, which accepts any confidential client, with a token
// that is not valid. Authentication succeeds or fails before the token
// matters, without needing a user or a service account.
func portalCredentials(ctx context.Context, idp *oidcclient.Client, in Input, introspect string) Result {
	if in.PortalSecret == "" {
		return Result{Portal, Skipped, "no portal client secret is configured; Keycloak's confidential portal client needs one"}
	}
	if !httpURL(introspect) {
		return Result{Portal, Skipped, "the provider publishes no introspection endpoint to verify client credentials"}
	}
	form := url.Values{"token": {"olr-connection-check"}, "token_type_hint": {"access_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, introspect, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{Portal, Failed, "the introspection endpoint URL is invalid"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(in.OIDC.PortalClientID), url.QueryEscape(in.PortalSecret))
	resp, err := idp.HTTP.Do(req)
	if err != nil {
		return Result{Portal, Failed, "the introspection endpoint could not be reached"}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusOK:
		return Result{Portal, Passed, "the provider accepted the portal client ID and secret"}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest:
		return Result{Portal, Failed, "the provider rejected the portal client ID or secret"}
	default:
		return Result{Portal, Failed, "the introspection endpoint answered with an unexpected status"}
	}
}

// token obtains one client-credentials access token.
func token(ctx context.Context, idp *oidcclient.Client, id, secret string) (string, error) {
	credentials := clientcredentials.Config{ClientID: id, ClientSecret: secret, TokenURL: idp.Provider.Endpoint().TokenURL}
	tok, err := credentials.Token(idp.Context(ctx))
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func tokenAudience(ctx context.Context, idp *oidcclient.Client, in Input) Result {
	if in.OIDC.Audience == "" {
		return Result{TokenAudience, Skipped, "no API audience is set; identity-provider tokens are not accepted on /v1"}
	}
	if in.TestClientID == "" || in.TestClientSecret == "" {
		return Result{TokenAudience, Skipped, "provide a test service client ID and secret to check service-token audience"}
	}
	raw, err := token(ctx, idp, in.TestClientID, in.TestClientSecret)
	if err != nil {
		return Result{TokenAudience, Failed, "the provider did not issue a token to the test service client; check its ID, secret and service-account setting"}
	}
	verifier := idp.Provider.Verifier(&oidc.Config{ClientID: in.OIDC.Audience})
	if _, err := verifier.Verify(idp.Context(ctx), raw); err != nil {
		if strings.Contains(err.Error(), "expected audience") {
			return Result{TokenAudience, Failed, "the test client's token does not name the API audience; add an audience mapper"}
		}
		return Result{TokenAudience, Failed, "the test client's token could not be verified for this issuer"}
	}
	return Result{TokenAudience, Passed, "a service token from the test client names the API audience"}
}

func syncPermissions(ctx context.Context, idp *oidcclient.Client, in Input) Result {
	if in.SyncSecret == "" {
		return Result{SyncAccess, Skipped, "group sync is off without a sync client secret"}
	}
	admin, err := groupsync.AdminURL(in.OIDC)
	if err != nil {
		return Result{SyncAccess, Failed, groupsync.ErrNotKeycloak.Error()}
	}
	raw, err := token(ctx, idp, in.OIDC.SyncClientID, in.SyncSecret)
	if err != nil {
		return Result{SyncAccess, Failed, "the provider rejected the sync client ID or secret, or the client has no service account"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, admin+"/users?max=1&briefRepresentation=true", nil)
	if err != nil {
		return Result{SyncAccess, Failed, "the admin API URL is invalid"}
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := idp.HTTP.Do(req)
	if err != nil {
		return Result{SyncAccess, Failed, "the Keycloak admin API could not be reached"}
	}
	defer resp.Body.Close()
	var users []json.RawMessage
	decoded := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&users) == nil
	switch {
	case resp.StatusCode == http.StatusOK && decoded:
		return Result{SyncAccess, Passed, "the sync service account can list users"}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Result{SyncAccess, Failed, "the sync service account cannot list users; grant it realm-management view-users"}
	default:
		return Result{SyncAccess, Failed, "the Keycloak admin API answered with an unexpected response"}
	}
}
