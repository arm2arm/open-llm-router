// Package testrequire defines fail-closed integration-test prerequisites.
package testrequire

import "fmt"

func Database(lookup func(string) string) error {
	if lookup("OLR_TEST_DATABASE_URL") == "" && (lookup("CI") == "true" || lookup("OLR_REQUIRE_DATABASE") == "1") {
		return fmt.Errorf("OLR_TEST_DATABASE_URL is required when CI=true or OLR_REQUIRE_DATABASE=1")
	}
	return nil
}
