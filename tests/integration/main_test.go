package integration_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/testrequire"
)

func TestMain(tests *testing.M) {
	if err := testrequire.Database(os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(tests.Run())
}
