package portal

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
)

type agentOperation struct {
	Name        string         `json:"name"`
	Permission  string         `json:"permission"`
	Additional  []string       `json:"additional_permissions,omitempty"`
	Write       bool           `json:"write"`
	Description string         `json:"description"`
	Params      []string       `json:"parameters"`
	BodySchema  map[string]any `json:"body_schema"`
	handler     func(http.ResponseWriter, *http.Request, user)
}
type agentArguments struct {
	Parameters map[string]string `json:"parameters,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func field(kind string) map[string]any { return map[string]any{"type": kind} }
func (h *Handler) operations() []agentOperation {
	empty := objectSchema(map[string]any{})
	rev := objectSchema(map[string]any{"version": field("integer"), "revision": field("integer")}, "version", "revision")
	document := objectSchema(map[string]any{"version": field("integer"), "document": field("object")}, "version", "document")
	// Rich configuration documents remain validated by the existing configuration services.
	endpoint := objectSchema(map[string]any{"model": field("string"), "revision": field("integer"), "configuration": field("object")}, "model", "revision", "configuration")
	keySchema := objectSchema(map[string]any{"label": field("string"), "permissions": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": adminaccess.Permissions}}, "allowed_networks": map[string]any{"type": "array", "items": field("string")}, "expires_in_days": field("integer")}, "label", "permissions", "allowed_networks")
	op := func(name, permission, description string, write bool, params []string, schema map[string]any, fn func(http.ResponseWriter, *http.Request, user)) agentOperation {
		return agentOperation{Name: name, Permission: permission, Description: description, Write: write, Params: params, BodySchema: schema, handler: fn}
	}
	ops := []agentOperation{
		op("get_service_status", "service:read", "Inspect replica health and capacity", false, nil, empty, h.adminReplicas),
		op("list_users", "users:read", "List users and usage; parameters.match optionally filters", false, []string{"match?"}, empty, h.adminUsers),
		op("get_user", "users:read", "Read one user's effective limits and key metadata", false, []string{"id"}, empty, h.adminUser),
		op("list_groups", "users:read", "Read group membership and usage totals", false, nil, empty, h.adminGroups),
		op("get_usage", "usage:read", "Read usage totals per group", false, nil, empty, h.adminGroups),
		op("get_admin_history", "audit:read", "Read paginated administration history", false, []string{"before?", "limit?", "action?", "actor?", "target?", "result?", "from?", "to?"}, empty, h.adminHistory),
		op("get_endpoints", "routing:read", "Read model endpoints and connection metadata", false, nil, empty, h.endpointList),
		op("get_routing", "routing:read", "Read routing configuration and revision state", false, nil, empty, h.routingSettings),
		op("preview_routing_policies", "routing:read", "Inspect the impact of hypothetical policies without applying them", false, nil, objectSchema(map[string]any{"revision": field("integer"), "document": field("object"), "user_id": field("string"), "memberships": map[string]any{"type": "array", "items": field("string")}}), h.previewPolicies),
		op("get_authentication", "authentication:read", "Read masked authentication settings", false, nil, empty, h.authenticationSettings),
		op("list_admin_keys", "admin_keys:read", "List your administration keys (metadata only)", false, nil, empty, func(w http.ResponseWriter, r *http.Request, u user) {
			keys, err := adminaccess.List(r.Context(), h.pool, u.ID)
			if err != nil {
				unavailable(w)
				return
			}
			respond(w, 200, map[string]any{"keys": keys})
		}),
		op("create_user", "users:write", "Create a local user with a temporary password", true, nil, objectSchema(map[string]any{"username": field("string"), "email": field("string"), "groups": map[string]any{"type": "array", "items": field("string")}, "password": field("string")}, "username", "groups", "password"), h.createLocalUser),
		op("set_user_groups", "users:write", "Assign local group memberships", true, []string{"id"}, objectSchema(map[string]any{"groups": map[string]any{"type": "array", "items": field("string")}}, "groups"), h.setLocalGroups),
		op("set_user_enabled", "users:write", "Enable or disable a user", true, []string{"id"}, objectSchema(map[string]any{"enabled": field("boolean")}, "enabled"), h.setEnabled),
		op("set_user_override", "users:write", "Set a quota or concurrency override", true, []string{"id", "field"}, objectSchema(map[string]any{"value": map[string]any{"type": []string{"integer", "null"}}, "reason": field("string")}, "value", "reason"), h.setOverride),
		op("clear_user_override", "users:write", "Remove an override and restore group policy", true, []string{"id", "field"}, empty, func(w http.ResponseWriter, r *http.Request, u user) {
			r.Method = http.MethodDelete
			h.setOverride(w, r, u)
		}),
		op("reset_user_password", "users:credentials", "Replace a local user's temporary password", true, []string{"id"}, objectSchema(map[string]any{"password": field("string")}, "password"), h.resetLocalPassword),
		op("revoke_user_key", "users:credentials", "Revoke an inference key", true, []string{"id", "key"}, empty, h.revokeUserKey),
		op("save_endpoint", "routing:write", "Save and apply endpoint configuration", true, nil, endpoint, h.endpointSave),
		op("restore_endpoint", "routing:write", "Restore endpoint configuration from file", true, nil, objectSchema(map[string]any{"model": field("string"), "revision": field("integer"), "target_revision": field("integer")}, "model", "revision", "target_revision"), h.endpointRestore),
		op("test_endpoint", "routing:write", "Probe a candidate endpoint; validates destination policy", true, nil, objectSchema(map[string]any{"model": field("string"), "configuration": field("object")}, "model", "configuration"), h.endpointTest),
		op("detect_models", "routing:write", "Detect upstream models using existing destination controls", true, nil, objectSchema(map[string]any{"model": field("string"), "replicas": map[string]any{"type": "array", "items": field("object")}}, "model", "replicas"), h.endpointModels),
		op("issue_admin_key", "admin_keys:write", "Issue a restricted child administration key; returned once", true, nil, keySchema, func(w http.ResponseWriter, r *http.Request, u user) {
			var body adminaccess.Create
			if !h.jsonBody(w, r, &body) {
				return
			}
			a := r.Context().Value(agentContext{}).(agentIdentity)
			h.issueAgentKey(w, r, u, body, &a.Key)
		}),
		op("revoke_admin_key", "admin_keys:write", "Revoke one of your administration keys", true, []string{"id"}, empty, h.agentRevoke),
	}
	ops = append(ops, op("narrow_admin_key", "admin_keys:write", "Reduce key permissions, networks or expiry; invalidates descendants", true, []string{"id"}, objectSchema(map[string]any{"permissions": map[string]any{"type": "array", "items": field("string")}, "allowed_networks": map[string]any{"type": "array", "items": field("string")}, "expires": field("string")}, "permissions", "allowed_networks", "expires"), h.agentNarrow))

	for _, domain := range []string{"routing", "authentication"} {
		permission := domain + ":write"
		var funcs []func(http.ResponseWriter, *http.Request, user)
		if domain == "routing" {
			funcs = []func(http.ResponseWriter, *http.Request, user){h.importRouting, h.saveRoutingDraft, h.discardRoutingDraft, h.checkRouting, h.activateRouting, h.rollbackRouting, h.applyRouting}
		} else {
			funcs = []func(http.ResponseWriter, *http.Request, user){h.importAuthentication, h.saveAuthenticationDraft, h.discardAuthenticationDraft, h.checkAuthentication, h.activateAuthentication, h.rollbackAuthentication, h.applyAuthentication}
		}
		for i, verb := range []string{"import", "save_draft", "discard_draft", "check", "activate", "rollback", "apply"} {
			schema := empty
			if i == 1 {
				schema = document
			}
			if i == 2 {
				schema = objectSchema(map[string]any{"version": field("integer")}, "version")
			}
			if domain == "authentication" && i == 1 {
				schema = objectSchema(map[string]any{"version": field("integer"), "document": field("object"), "secrets": field("object")}, "version", "document")
			}
			if i == 3 && domain == "authentication" {
				schema = objectSchema(map[string]any{"revision": field("integer"), "test_client_id": field("string"), "test_client_secret": field("string")}, "revision")
			}
			if i == 3 && domain == "routing" {
				schema = objectSchema(map[string]any{"revision": field("integer")}, "revision")
			}
			if i == 4 || i == 5 {
				schema = rev
			}
			ops = append(ops, op(verb+"_"+domain, permission, verb+" "+domain+" configuration using existing revision checks", true, nil, schema, funcs[i]))
		}
	}
	for i := range ops {
		if strings.HasSuffix(ops[i].Permission, ":write") {
			ops[i].Additional = append(ops[i].Additional, strings.TrimSuffix(ops[i].Permission, ":write")+":read")
		}
		if ops[i].Permission == "users:credentials" {
			ops[i].Additional = append(ops[i].Additional, "users:read")
		}

		if ops[i].Name == "issue_admin_key" {
			ops[i].Additional = append(ops[i].Additional, "admin_keys:delegate")
		}
		// Group assignments and policy changes can grant administrative authority.
		if slices.Contains([]string{"create_user", "set_user_groups", "save_draft_routing", "activate_routing", "rollback_routing", "apply_routing", "import_routing"}, ops[i].Name) {
			ops[i].Additional = append(ops[i].Additional, "admin_keys:delegate")
		}
		if ops[i].Name == "create_user" {
			ops[i].Additional = append(ops[i].Additional, "users:credentials")
		}
	}
	return ops
}
func (op agentOperation) allowed(a agentIdentity) bool {
	if !a.Key.Allows(op.Permission) {
		return false
	}
	for _, p := range op.Additional {
		if !a.Key.Allows(p) {
			return false
		}
	}
	return true
}
func (h *Handler) operation(name string) (agentOperation, bool) {
	for _, op := range h.operations() {
		if op.Name == name {
			return op, true
		}
	}
	return agentOperation{}, false
}
func (h *Handler) agentOperations(w http.ResponseWriter, _ *http.Request, a agentIdentity) {
	ops := []agentOperation{}
	for _, op := range h.operations() {
		if op.allowed(a) {
			ops = append(ops, op)
		}
	}
	respond(w, 200, map[string]any{"operations": ops, "authentication_test_sign_in_url": h.cfg.Portal.BaseURL + "/portal/admin#authentication"})
}
func decodeAgent(w http.ResponseWriter, r *http.Request, value any) bool {
	r2 := r.Clone(r.Context())
	r2.Header = r.Header.Clone()
	// The origin bypass is scoped to the authenticated agent context, never a header.
	h := &Handler{}
	return h.jsonBodyLimit(w, r2, value, 1<<20)
}
func (h *Handler) agentRead(w http.ResponseWriter, r *http.Request, a agentIdentity) {
	op, ok := h.operation(r.PathValue("operation"))
	if !ok || op.Write {
		fail(w, 404, "not_found_error", "read operation not found")
		return
	}
	if !op.allowed(a) {
		h.agentAudit(r, a, "agent.read", "refused", "")
		fail(w, 403, "permission_error", "key lacks required permission")
		return
	}
	var args agentArguments
	if !decodeAgent(w, r, &args) {
		return
	}
	if !validArgs(w, op, args) {
		return
	}
	h.invokeAgent(w, r, a, op, args)
}
func validArgs(w http.ResponseWriter, op agentOperation, args agentArguments) bool {
	allowed := map[string]bool{}
	for _, name := range op.Params {
		optional := strings.HasSuffix(name, "?")
		name = strings.TrimSuffix(name, "?")
		allowed[name] = true
		if !optional && args.Parameters[name] == "" {
			fail(w, 400, "invalid_request_error", "missing parameter: "+name)
			return false
		}
	}
	for name := range args.Parameters {
		if !allowed[name] {
			fail(w, 400, "invalid_request_error", "unknown parameter: "+name)
			return false
		}
	}
	if len(args.Body) == 0 {
		args.Body = json.RawMessage(`{}`)
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(args.Body))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil || body == nil {
		fail(w, 400, "invalid_request_error", "body must be an object")
		return false
	}
	props := op.BodySchema["properties"].(map[string]any)
	for name := range body {
		if schema, ok := props[name]; ok {
			if !schemaValue(body[name], schema.(map[string]any)) {
				fail(w, 400, "invalid_request_error", "invalid body field type: "+name)
				return false
			}
		} else {
			fail(w, 400, "invalid_request_error", "unknown body field: "+name)
			return false
		}
	}
	if required, ok := op.BodySchema["required"].([]string); ok {
		for _, name := range required {
			if _, ok := body[name]; !ok {
				fail(w, 400, "invalid_request_error", "missing body field: "+name)
				return false
			}
		}
	}
	return true
}
func (h *Handler) invokeAgent(w http.ResponseWriter, r *http.Request, a agentIdentity, op agentOperation, args agentArguments) {
	if (strings.Contains(op.Name, "routing") && (h.Routing == nil || h.RoutingRuntime == nil)) || (strings.Contains(op.Name, "authentication") && h.Settings == nil) || (strings.Contains(op.Name, "endpoint") && h.Endpoints == nil) {
		unavailable(w)
		return
	}
	body := args.Body
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	r2 := r.Clone(r.Context())
	r2.Method = "POST"
	r2.Header = r.Header.Clone()
	r2.Header.Set("Content-Type", "application/json")
	r2.Body = io.NopCloser(bytes.NewReader(body))
	query := url.Values{}
	for k, v := range args.Parameters {
		r2.SetPathValue(k, v)
		query.Set(k, v)
	}
	r2.URL = new(url.URL)
	*r2.URL = *r.URL
	r2.URL.RawQuery = query.Encode()
	op.handler(w, r2, a.User)
}

// captureAgent intentionally never retains handler response bodies in persistent state.
func (h *Handler) captureAgent(r *http.Request, a agentIdentity, op agentOperation, args agentArguments) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.invokeAgent(w, r, a, op, args)
	return w
}

// schemaValue performs structural validation before a preview is persisted.
// Domain services perform semantic checks again at application time.
func schemaValue(value any, schema map[string]any) bool {
	matches := func(kind string) bool {
		switch kind {
		case "null":
			return value == nil
		case "string":
			_, ok := value.(string)
			return ok
		case "boolean":
			_, ok := value.(bool)
			return ok
		case "integer":
			n, ok := value.(json.Number)
			if !ok {
				return false
			}
			_, err := n.Int64()
			return err == nil
		case "array":
			_, ok := value.([]any)
			return ok
		case "object":
			_, ok := value.(map[string]any)
			return ok
		}
		return false
	}
	if kinds, ok := schema["type"].([]string); ok {
		valid := false
		for _, kind := range kinds {
			valid = valid || matches(kind)
		}
		if !valid {
			return false
		}
	} else if kind, ok := schema["type"].(string); ok && !matches(kind) {
		return false
	}
	if entries, ok := value.([]any); ok {
		if item, ok := schema["items"].(map[string]any); ok {
			for _, v := range entries {
				if !schemaValue(v, item) {
					return false
				}
			}
		}
	}
	if choices, ok := schema["enum"].([]string); ok {
		str, ok := value.(string)
		if !ok || !slices.Contains(choices, str) {
			return false
		}
	}
	return true
}
