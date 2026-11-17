package testrequire

import (
	"strings"
	"testing"
)

func TestDatabaseRequirement(t *testing.T) {
	for _, env := range []map[string]string{{}, {"CI": "true"}, {"OLR_REQUIRE_DATABASE": "1"}, {"CI": "true", "OLR_TEST_DATABASE_URL": "synthetic"}} {
		err := Database(func(name string) string { return env[name] })
		required := env["OLR_TEST_DATABASE_URL"] == "" && (env["CI"] == "true" || env["OLR_REQUIRE_DATABASE"] == "1")
		if (err != nil) != required || err != nil && !strings.Contains(err.Error(), "OLR_TEST_DATABASE_URL") {
			t.Fatal("unexpected prerequisite result", err)
		}
	}
}
