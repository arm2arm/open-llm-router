// Package audit defines administration audit events. An event identifies who
// did what to which record and with what result. It never carries passwords,
// tokens, hashes, secret values or request content; callers pass stable IDs
// and sanitized error messages only.
//
// Events are currently written as structured log lines. The persistent,
// append-only audit store (#77/#84) consumes the same shape.
package audit

import (
	"io"
	"log/slog"
	"os"
	"os/user"
	"strconv"
	"time"
)

// Result values. A refused action was rejected by a policy or validation
// check; a failed one could not complete (input, database or invalidation).
const (
	Succeeded = "succeeded"
	Refused   = "refused"
	Failed    = "failed"
)

type Actor struct {
	Type string `json:"type"`           // e.g. operator_cli
	Name string `json:"name,omitempty"` // OS account running the command
	Host string `json:"host,omitempty"`
}

type Event struct {
	Time     time.Time        `json:"time"`
	Action   string           `json:"action"`
	Actor    Actor            `json:"actor"`
	TargetID string           `json:"target_id,omitempty"`
	Result   string           `json:"result"`
	Reason   string           `json:"reason,omitempty"`
	Details  map[string]int64 `json:"details,omitempty"`
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
