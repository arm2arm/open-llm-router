// Package reqid carries the ID of the current request in a context, so that
// any package can put it into its log lines without importing the server.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/arm2arm/open-llm-router/internal/usage"
)

type key struct{}
type detailsKey struct{}

// Details is metadata owned by the request handler. It never carries content
// or credentials. The outer server reads it after downstream handlers finish.
type Details struct {
	UserID, KeyPrefix, TraceID string
	Record                     usage.Record
}

func DetailsFromContext(ctx context.Context) *Details {
	d, _ := ctx.Value(detailsKey{}).(*Details)
	return d
}

// New returns a random request ID: 32 hex characters.
func New() string {
	var b [16]byte
	rand.Read(b[:]) // never fails; see crypto/rand
	return hex.EncodeToString(b[:])
}

// NewContext returns a copy of ctx that carries id.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(context.WithValue(ctx, key{}, id), detailsKey{}, &Details{})
}

// FromContext returns the request ID in ctx, or "" if there is none.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}
