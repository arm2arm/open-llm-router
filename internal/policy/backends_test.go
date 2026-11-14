package policy

import (
	"github.com/arm2arm/open-llm-router/internal/config"
	"testing"
)

func TestBackendVisibilityWithWildcardAndMixedPools(t *testing.T) {
	cfg := &config.Config{
		Groups:   map[string]config.Group{"staff": {Models: []string{"*"}}, "students": {Models: []string{"*"}}, "admins": {Admin: true}},
		Backends: map[string]config.BackendPolicy{"private": {Groups: []string{"staff"}}, "shared": {Groups: []string{"students"}}, "legacy": {}, "closed": {Groups: []string{}}},
		Models: map[string]config.Model{
			"private": {Pool: []config.Replica{{Backend: "private"}}},
			"mixed":   {Pool: []config.Replica{{Backend: "private"}, {Backend: "shared"}}},
			"legacy":  {Pool: []config.Replica{{Backend: "legacy"}}},
			"closed":  {Pool: []config.Replica{{Backend: "closed"}}},
		},
	}
	for _, tc := range []struct {
		group, model string
		want         bool
	}{
		{"students", "private", false}, {"staff", "private", true}, {"students", "mixed", true},
		{"students", "legacy", true}, {"students", "closed", false}, {"admins", "closed", true},
		{"staff", "missing", false},
	} {
		access := ForUser(cfg.Groups, []string{tc.group}, nil, false)
		if got := ModelAllowed(cfg, access, tc.model, []string{tc.group}, false); got != tc.want {
			t.Errorf("%s/%s = %v", tc.group, tc.model, got)
		}
	}
	if BackendAllowed(cfg, "private", []string{"students"}, false) {
		t.Fatal("mixed pool allowed private replica")
	}
	if !BackendAllowed(cfg, "closed", nil, true) {
		t.Fatal("protected administrator denied")
	}
}
