// Package audit defines administration audit events. An event identifies who
// did what to which record and with what result. It never carries passwords,
// tokens, hashes, secret values or request content; callers pass stable IDs
// and sanitized error messages only.
//
// Events are written as structured log lines (Write) and, for changes made in
// a database transaction, to the append-only audit_events table (Record) in
// that same transaction, so a committed change always has its event.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Result values. A refused action was rejected by a policy or validation
// check; a failed one could not complete (input, database or invalidation).
const (
	Succeeded = "succeeded"
	Refused   = "refused"
	Failed    = "failed"
)

type Actor struct {
	Type string `json:"type"`           // operator_cli or portal_user
	ID   string `json:"id,omitempty"`   // router user UUID, for portal users
	Name string `json:"name,omitempty"` // OS account or username
	Host string `json:"host,omitempty"`
}

// Label is a short attribution for revision and activation rows.
func (a Actor) Label() string {
	label := a.Type
	if a.ID != "" {
		label += ":" + a.ID
	}
	if a.Name != "" {
		label += ":" + a.Name
	}
	if a.Host != "" {
		label += "@" + a.Host
	}
	return label
}

type Event struct {
	Time       time.Time      `json:"time"`
	Action     string         `json:"action"`
	Actor      Actor          `json:"actor"`
	TargetType string         `json:"target_type,omitempty"`
	TargetID   string         `json:"target_id,omitempty"`
	Revision   int64          `json:"revision,omitempty"`
	Result     string         `json:"result"`
	Reason     string         `json:"reason,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

// OperatorCLI identifies the host/container account running an olr command.
// Database access is the authority for these commands, so this is attribution,
// not authentication.
func OperatorCLI() Actor {
	actor := Actor{Type: "operator_cli", Name: "uid:" + strconv.Itoa(os.Getuid())}
	if u, err := user.Current(); err == nil && u.Username != "" {
		actor.Name = u.Username
	}
	if host, err := os.Hostname(); err == nil {
		actor.Host = host
	}
	return actor
}

// Write emits one JSON log line, message "admin audit", with the event nested
// under "event" in the shape of its JSON tags.
func Write(w io.Writer, e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	slog.New(slog.NewJSONHandler(w, nil)).Info("admin audit", "event", e)
}

type executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Record inserts the event with the caller's transaction (or pool, for an
// event that must outlive a rolled-back change). Details must already be
// redacted: field names, counts and IDs, never values of secrets.
func Record(ctx context.Context, db executor, e Event) error {
	var details []byte
	if len(e.Details) > 0 {
		var err error
		if details, err = json.Marshal(e.Details); err != nil {
			return errors.New("encode audit details failed")
		}
	}
	var revision *int64
	if e.Revision > 0 {
		revision = &e.Revision
	}
	_, err := db.Exec(ctx, `INSERT INTO audit_events(action,actor_type,actor_id,actor_name,actor_host,target_type,target_id,revision,result,reason,details)
		VALUES($1,$2,NULLIF($3,'')::uuid,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),$8,$9,NULLIF($10,''),$11)`,
		e.Action, e.Actor.Type, e.Actor.ID, e.Actor.Name, e.Actor.Host, e.TargetType, e.TargetID, revision, e.Result, e.Reason, details)
	if err != nil {
		return errors.New("audit event could not be recorded")
	}
	return nil
}

type actorKey struct{}

// WithActor carries authenticated attribution into shared mutation services.
func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}
func ActorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorKey{}).(Actor)
	return actor, ok
}
