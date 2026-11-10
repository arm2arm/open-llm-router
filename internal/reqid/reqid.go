// Package reqid carries the ID of the current request in a context, so that
// any package can put it into its log lines without importing the server.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type key struct{}

// New returns a random request ID: 32 hex characters.
func New() string {
	var b [16]byte
	rand.Read(b[:]) // never fails; see crypto/rand
	return hex.EncodeToString(b[:])
}

// NewContext returns a copy of ctx that carries id.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// FromContext returns the request ID in ctx, or "" if there is none.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}
