package integration_test

import (
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/arm2arm/open-llm-router/internal/testrequire"
)

func TestMain(tests *testing.M) {
	if err := testrequire.Database(os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(tests.Run())
}

// backendProbe reports a request the router makes on its own: a health probe
// or a model catalog read for the context length.
func backendProbe(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == pool.CatalogPath)
}
