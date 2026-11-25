"""Issue 246: independent security guards and the tests that must kill them.

SQL mutations keep bind arguments valid. Conditions retain variable references
so a compiler error cannot masquerade as a security assertion.
"""

CASES = []


def case(name, package, test, *edits):
    CASES.append(dict(name=name, package=package, test=test, edits=edits))


def integration(name, test, *edits):
    case(name, "./tests/integration", test, *edits)


store = "internal/adminaccess/store.go"
agent = "internal/portal/agent.go"
changes = "internal/portal/agent_changes.go"
http_test = "TestAgentHTTPAuthorizationAndPreviewBoundaries"
key_test = "TestAdministrationKeySecurityBoundaries"

case("caller-authorization", "./internal/proxy", "TestKeylessBackendNeverReceivesCallerCredentials",
     ("internal/proxy/proxy.go", '[]string{"Authorization", "X-Api-Key", "Cookie",', '[]string{"X-Api-Key", "Cookie",'))
integration("read-never-writes", http_test,
            ("internal/portal/agent_operations.go", "if !ok || op.Write {", "if !ok {"))
integration("narrow-expiry", key_test, (store, "k.Revoked || expires.After(k.Expires) ||", "k.Revoked ||"))
integration("narrow-network", key_test, (store, " || !Subnets(networks, k.Networks)", ""))
integration("key-and-ancestor-expiry", key_test, (store, "c.expires>clock_timestamp() AND ", ""))
integration("admin-api-off-switch", "TestDisabledAgentAPIRefusesValidAdministrationKey",
            (agent, "if !h.cfg.AdminAPI.Enabled {", "if false && !h.cfg.AdminAPI.Enabled {", 2))
integration("token-cache-time", "TestAccessTokenCacheCannotOutliveToken",
            ("internal/apitoken/apitoken.go", " && v.now().Before(cached.until)", ""))
integration("token-cache-expiry-clamp", "TestAccessTokenCacheCannotOutliveToken",
            ("internal/apitoken/apitoken.go", "if token.Expiry.Before(until) {", "if false && token.Expiry.Before(until) {"))
integration("child-permission-subset", key_test,
            (store, "if !parent.Allows(p) {", "if false && !parent.Allows(p) {"))
integration("child-delegation-both-layers", key_test,
            (store, '!parent.Allows("admin_keys:delegate") || ', ""),
            ("internal/portal/agent_operations.go", 'if ops[i].Name == "issue_admin_key" {', 'if false && ops[i].Name == "issue_admin_key" {'))
integration("strict-quota-reservation", "TestStrictQuotaShapesNeverDispatchUnreservedWork",
            ("internal/proxy/proxy.go", "admission.Reserve = meta.MaxTokens + int64(len(body))", "admission.Reserve = 0"))
case("forwarded-header-lines", "./internal/adminaccess", "TestForwardedHeaderLinesCannotChooseAnAllowlistedAddress",
     ("internal/adminaccess/ip.go", "len(headers) != 1", "len(headers) == 0"))
integration("preview-key-binding", http_test,
            (changes, "FROM admin_changes WHERE id=$1 AND key_id=$2`, id, a.Key.ID).Scan(&name, &status, &expected", "FROM admin_changes WHERE id=$1 AND $2<>''`, id, a.Key.ID).Scan(&name, &status, &expected"))
integration("preview-policy-version", http_test,
            (changes, " || policy != a.Key.PolicyVersion", " || (false && policy != a.Key.PolicyVersion)"))
for method, handler in [("GET", "agentKeys"), ("POST", "agentIssue"), ("PUT", "agentNarrow"), ("DELETE", "agentRevoke")]:
    integration("portal-agent-keys-admin-" + method.lower(), http_test,
                (agent, f"h.admin(h.{handler})", f"h.authenticated(h.{handler})"))
integration("admin-key-revoke-owner", key_test,
            (store, "WHERE owner_id=$1 AND id=$2`, owner, id)", "WHERE $1<>'' AND id=$2`, owner, id)"))
integration("expired-key-delete-owner", "TestExpiredKeyDeletionOwnerAndHistory",
            ("internal/auth/store.go", "DELETE FROM api_keys WHERE id=$1 AND user_id=$2", "DELETE FROM api_keys WHERE id=$1 AND $2<>''"))
case("delegated-prefix-length", "./internal/adminaccess", "TestDelegationCannotWidenPrefixAtSameNetworkAddress",
     ("internal/adminaccess/ip.go", "p.Bits() <= c.Bits() && ", ""))
integration("active-key-cap", key_test, (store, "if count >= maxActive {", "if false && count >= maxActive {"))
case("key-lifetime-bounds", "./internal/adminaccess", "TestAdministrationKeyLifetimeBounds",
     (store, " || c.Days < 1 || c.Days > maxDays", ""))

issuance = "internal/portal/user_keys.go"
issuance_test = "TestUserKeyIssuanceForExistingAndKeycloakUsers"
integration("issued-key-revoke-issuer", issuance_test,
            ("internal/auth/store.go", "WHERE id=$1 AND issued_by_admin_key=$2 RETURNING", "WHERE id=$1 AND $2<>'' RETURNING"))
integration("issued-key-summary-issuer", issuance_test,
            (issuance, "WHERE k.id=$1 AND k.issued_by_admin_key=$2`", "WHERE k.id=$1 AND $2<>''`"))
integration("issued-key-oidc-issuer-only", issuance_test,
            (issuance, 'if issuer == "" || issuer == "local" || body.Issuer != issuer {', 'if issuer == "" {'))
integration("issued-key-disabled-user", issuance_test,
            (issuance, "if !t.Enabled {", "if false && !t.Enabled {"))
integration("issued-key-no-model-access", issuance_test,
            (issuance, "if len(userModelEntries(", "if false && len(userModelEntries("))
integration("issued-key-keycloak-subject", issuance_test,
            ("internal/groupsync/groupsync.go", "if remote.ID != sub {", "if false && remote.ID != sub {"))
integration("issued-key-allowed-groups", "TestUserKeyIssuanceSiteLimits",
            (issuance, "len(allowed) > 0 && ", "false && "))
integration("issued-key-site-lifetime", "TestUserKeyIssuanceSiteLimits",
            (issuance, "limit > 0 && limit < maximum {", "false && limit < maximum {"))
integration("issued-key-permission-filtering", "TestUserKeyIssuanceDiscoveryIsPermissionFiltered",
            ("internal/portal/agent_operations.go", "func (op agentOperation) allowed(a agentIdentity) bool {", "func (op agentOperation) allowed(a agentIdentity) bool {\n\tif op.Permission == \"user_keys:issue\" {\n\t\treturn true\n\t}"))

audit_tests = {
    "key.create": "TestExpiredKeyDeletionOwnerAndHistory",
    "key.delete": "TestExpiredKeyDeletionOwnerAndHistory",
    "credential.rotate": key_test,
    "local_admin.bootstrap": key_test,
    "user.groups": "TestExpiredKeyDeletionOwnerAndHistory",
    "user.identity_sync": "TestExpiredKeyDeletionOwnerAndHistory",
    **{action: "TestAgentAdministrationLifecycle" for action in ["agent.preview", "agent.apply", "agent.read", "agent.access"]},
    **{action: key_test for action in ["admin_key.issue", "admin_key.revoke", "admin_key.narrow"]},
    "settings.apply": "TestAdminRoutingRevisions",
    "settings.validate": "TestAdminRoutingRevisions",
    "endpoints.activate": "TestEndpointConfigurationLifecyclePersistenceAndGuards",
    "user_key.issue": issuance_test,
    "user_key.revoke": issuance_test,
}
for action, test in audit_tests.items():
    signature = "func Record(ctx context.Context, db executor, e Event) error {"
    integration("history-" + action, test,
                ("internal/audit/audit.go", signature, signature + f'\n\tif e.Action == "{action}" {{ return nil }}'))

integration("routing-admin-demotion-all-layers", "TestLegacyRoutingRollbackKeepsDeploymentAdministratorsKeys",
            ("internal/db/migrations/0016_admin_agent_access.sql", "IF NEW.domain='routing' AND", "IF false AND NEW.domain='routing' AND"),
            ("internal/db/migrations/0029_legacy_routing_administration.sql", "IF NEW.domain='routing' AND", "IF false AND NEW.domain='routing' AND"),
            ("internal/portal/routingsettings.go", "if !h.access(user{ID: id, Groups: groups, ProtectedAdmin: protected}).Admin {", "if false && !h.access(user{ID: id, Groups: groups, ProtectedAdmin: protected}).Admin {"),
            (agent, "if !h.access(u).Admin {", "if false && !h.access(u).Admin {"))
integration("oidc-access-token-hash", "TestPortalOIDCStateReplayAndTokenValidation",
            ("internal/portal/oauth.go", 'if id.AccessTokenHash != "" &&', 'if false && id.AccessTokenHash != "" &&'))
integration("duplicate-credentials", "TestDuplicateAuthorizationCannotHideBesideValidKey",
            ("internal/auth/auth.go", "if len(auth) > 1 || len(api) > 1 {", "if false && (len(auth) > 1 || len(api) > 1) {"))
integration("portal-json-body-limit", http_test,
            ("internal/portal/portal.go", "json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))", "json.NewDecoder(r.Body)"))
integration("portal-session-issuer", "TestPortalSessionIssuerIsCheckedWithoutCleanup",
            ("internal/portal/session.go", "u.issuer=$2", "$2<>''"))
