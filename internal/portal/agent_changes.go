package portal

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/jackc/pgx/v5"
)

type agentChangeRequest struct {
	Operation string         `json:"operation"`
	Arguments agentArguments `json:"arguments"`
}

func sanitizeAgent(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for name, item := range v {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "authorization") || strings.Contains(lower, "credential") {
				out[name] = "will change"
			} else {
				out[name] = sanitizeAgent(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sanitizeAgent(item)
		}
		return out
	default:
		return value
	}
}
func (h *Handler) agentPreview(w http.ResponseWriter, r *http.Request, a agentIdentity) {
	var body agentChangeRequest
	if !decodeAgent(w, r, &body) {
		return
	}
	op, ok := h.operation(body.Operation)
	if !ok || !op.Write {
		fail(w, 400, "invalid_request_error", "unknown change operation")
		return
	}
	if !op.allowed(a) {
		h.agentAudit(r, a, "agent.preview", audit.Refused, "")
		fail(w, 403, "permission_error", "key lacks required permissions")
		return
	}
	if !validArgs(w, op, body.Arguments) {
		return
	}
	raw, _ := json.Marshal(body.Arguments)
	hash, err := argumentHash(raw)
	if err != nil {
		fail(w, 400, "invalid_request_error", "invalid arguments")
		return
	}
	var summary any
	_ = json.Unmarshal(raw, &summary)
	summary = sanitizeAgent(summary)
	// Authentication documents may contain secret material in arbitrary extension fields.
	if strings.Contains(op.Name, "authentication") {
		summary = map[string]any{"operation": op.Name, "configuration": "authentication settings will change; secret values omitted"}
	}
	// Bind the snapshot to the state observed before any reads. A concurrent
	// browser, CLI or replica write must never bless an outdated before-image.
	var version int64
	if err := h.pool.QueryRow(r.Context(), `SELECT version FROM admin_state WHERE id=true`).Scan(&version); err != nil {
		unavailable(w)
		return
	}
	var before any
	readName := ""
	switch {
	case strings.Contains(op.Name, "user"):
		if body.Arguments.Parameters["id"] != "" {
			readName = "get_user"
		}
	case strings.Contains(op.Name, "authentication"):
		readName = "get_authentication"
	case strings.Contains(op.Name, "routing"):
		readName = "get_routing"
	case strings.Contains(op.Name, "endpoint"):
		readName = "get_endpoints"
	case strings.Contains(op.Name, "admin_key"):
		readName = "list_admin_keys"
	}
	if read, exists := h.operation(readName); exists && read.allowed(a) {
		args := agentArguments{Parameters: map[string]string{}}
		if readName == "get_user" {
			args.Parameters["id"] = body.Arguments.Parameters["id"]
		}
		response := h.captureAgent(r, a, read, args)
		if response.Code < 400 {
			_ = json.Unmarshal(response.Body.Bytes(), &before)
			before = sanitizeAgent(before)
		}
	}
	summary = map[string]any{"before": before, "after": summary, "description": op.Description}
	summaryRaw, _ := json.Marshal(summary)
	var id string
	var expires time.Time
	errPreviewStale := errors.New("administration state changed while preparing preview")
	err = pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		var current int64
		if err := tx.QueryRow(r.Context(), `SELECT version FROM admin_state WHERE id=true FOR SHARE`).Scan(&current); err != nil {
			return err
		}
		if current != version {
			return errPreviewStale
		}
		if err := tx.QueryRow(r.Context(), `INSERT INTO admin_changes(key_id,operation,arguments_hash,state_version,policy_version,summary) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text,expires`, a.Key.ID, op.Name, hash, version, a.Key.PolicyVersion, summaryRaw).Scan(&id, &expires); err != nil {
			return err
		}
		actor := *a.User.AgentActor
		actor.ChangeID = id
		return audit.Record(r.Context(), tx, audit.Event{Action: "agent.preview", Actor: actor, TargetType: "admin_change", TargetID: id, Result: audit.Succeeded})
	})
	if errors.Is(err, errPreviewStale) {
		fail(w, 409, "conflict_error", "administration state changed; request a new preview")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	respond(w, 201, map[string]any{"id": id, "operation": op.Name, "proposed": summary, "state_version": version, "expires": expires, "required_permissions": append([]string{op.Permission}, op.Additional...), "warnings": []string{"Preview does not apply changes. Apply revalidates configuration and authorization.", "Changes are autonomous with this key; this preview is not a human approval."}})
}
func (h *Handler) agentApply(w http.ResponseWriter, r *http.Request, a agentIdentity) {
	var body struct {
		Arguments      agentArguments `json:"arguments"`
		IdempotencyKey string         `json:"idempotency_key"`
	}
	if !decodeAgent(w, r, &body) {
		return
	}
	if len(body.IdempotencyKey) < 8 || len(body.IdempotencyKey) > 128 {
		fail(w, 400, "invalid_request_error", "idempotency_key must contain 8 to 128 characters")
		return
	}
	id, ok := pathUUID(w, r, "id", "change not found")
	if !ok {
		return
	}
	h.agentMu.Lock()
	defer h.agentMu.Unlock()
	// Authentication is repeated after acquiring the execution lock.
	fresh, err := h.agentIdentity(r)
	if err != nil {
		fail(w, 403, "permission_error", "administration authorization changed")
		return
	}
	a = fresh
	raw, _ := json.Marshal(body.Arguments)
	hash, _ := argumentHash(raw)
	var name, status string
	var expected []byte
	var state, policy, current int64
	var expires time.Time
	err = h.pool.QueryRow(r.Context(), `SELECT operation,status,arguments_hash,state_version,policy_version,expires FROM admin_changes WHERE id=$1 AND key_id=$2`, id, a.Key.ID).Scan(&name, &status, &expected, &state, &policy, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "not_found_error", "change not found")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	if status != "previewed" {
		h.agentChange(w, r, a)
		return
	}
	op, ok := h.operation(name)
	if !ok || !op.allowed(a) {
		fail(w, 403, "permission_error", "key lacks required permission")
		return
	}
	if time.Now().After(expires) || policy != a.Key.PolicyVersion || subtle.ConstantTimeCompare(hash, expected) != 1 {
		fail(w, 409, "preview_conflict", "preview expired or arguments or policy changed; create a new preview")
		return
	}
	if !validArgs(w, op, body.Arguments) {
		return
	}
	err = h.pool.QueryRow(r.Context(), `SELECT version FROM admin_state WHERE id=true`).Scan(&current)
	if err != nil {
		unavailable(w)
		return
	}
	if state != current {
		fail(w, 409, "preview_conflict", "administration state changed; create a new preview")
		return
	}
	result, err := h.pool.Exec(r.Context(), `UPDATE admin_changes SET status='running',idempotency_key=$3 WHERE id=$1 AND key_id=$2 AND status='previewed' AND state_version=(SELECT version FROM admin_state WHERE id=true)`, id, a.Key.ID, body.IdempotencyKey)
	if err != nil {
		fail(w, 409, "idempotency_conflict", "idempotency key already used or change unavailable")
		return
	}
	if result.RowsAffected() != 1 {
		fail(w, 409, "preview_conflict", "administration state changed; create a new preview")
		return
	}
	actor := *a.User.AgentActor
	actor.ChangeID = id
	a.User.AgentActor = &actor
	r = r.WithContext(audit.WithActor(r.Context(), actor))
	response := h.captureAgent(r, a, op, body.Arguments)
	final, resultName := "succeeded", audit.Succeeded
	if response.Code >= 400 {
		final, resultName = "failed", audit.Failed
	}
	// Finalize with a fresh context even when the caller disconnects after committing.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	_, err = h.pool.Exec(ctx, `UPDATE admin_changes SET status=$2,response_status=$3,completed=clock_timestamp() WHERE id=$1`, id, final, response.Code)
	if err != nil {
		fail(w, 503, "outcome_uncertain", "inspect change status and target before taking further action")
		return
	}
	h.agentAudit(r, a, "agent.apply", resultName, id)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Admin-Change-Id", id)
	w.WriteHeader(response.Code)
	_, _ = w.Write(response.Body.Bytes())
}
func (h *Handler) agentChange(w http.ResponseWriter, r *http.Request, a agentIdentity) {
	id, ok := pathUUID(w, r, "id", "change not found")
	if !ok {
		return
	}
	var name, status string
	var code *int
	var summary json.RawMessage
	var created, expires time.Time
	var completed *time.Time
	err := h.pool.QueryRow(r.Context(), `SELECT operation,status,response_status,summary,created,expires,completed FROM admin_changes WHERE id=$1 AND key_id=$2`, id, a.Key.ID).Scan(&name, &status, &code, &summary, &created, &expires, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "not_found_error", "change not found")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	if status == "running" {
		status = "outcome_uncertain"
	}
	respond(w, 200, map[string]any{"id": id, "operation": name, "status": status, "response_status": code, "proposed": json.RawMessage(bytes.Clone(summary)), "created": created, "expires": expires, "completed": completed, "secret_recoverable": false})
}
