package db

import (
	"strings"
	"testing"
)

func TestInvalidConnectionString(t *testing.T) {
	for _, dsn := range []string{"", " \t", "postgres://user:private-password@host:invalid/db"} {
		pool, err := Open(t.Context(), dsn)
		if pool != nil || err == nil {
			t.Fatalf("invalid connection string: pool=%v, err=%v", pool, err)
		}
		if strings.Contains(err.Error(), "private-password") {
			t.Fatal("parse error disclosed database credentials")
		}
	}
}
