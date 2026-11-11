package policy

import (
	"reflect"
	"slices"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func ptr[T any](value T) *T { return &value }

func TestResolveModels(t *testing.T) {
	groups := map[string]config.Group{
		"students": {Models: []string{"chat", "shared"}},
		"staff":    {Models: []string{"large", "shared"}},
		"admins":   {Models: []string{"*"}},
	}
	for _, tc := range []struct {
		name        string
		memberships []string
		allowed     []string
	}{
		{"no memberships", nil, nil},
		{"unconfigured", []string{"unknown"}, nil},
		{"exact names", []string{"Students", "/students"}, nil},
		{"single group", []string{"students"}, []string{"chat", "shared"}},
		{"union", []string{"students", "staff"}, []string{"chat", "shared", "large"}},
		{"unknown ignored", []string{"unknown", "students"}, []string{"chat", "shared"}},
		{"duplicates", []string{"students", "students"}, []string{"chat", "shared"}},
		{"wildcard", []string{"students", "admins"}, []string{"chat", "shared", "large", "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Resolve(groups, tc.memberships)
			for _, model := range []string{"chat", "shared", "large", "other"} {
				if got := p.AllowsModel(model); got != slices.Contains(tc.allowed, model) {
					t.Errorf("AllowsModel(%q) = %t", model, got)
				}
			}
		})
	}
	if Resolve(nil, []string{"admins"}).AllowsModel("chat") || (Policy{}).AllowsModel("chat") {
		t.Fatal("absent policies must deny access")
	}
}

func TestResolveLimits(t *testing.T) {
	groups := map[string]config.Group{
		"students": {Models: []string{"chat"}, TokensPerDay: ptr(int64(300_000)), TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(20), MaxConcurrent: ptr(2)},
		"staff":    {Models: []string{"large"}, TokensPerDay: ptr(int64(2_000_000)), TokensPerMonth: ptr(int64(4_000_000)), RPM: ptr(60), MaxConcurrent: ptr(1)},
		"partial":  {Models: []string{"chat"}, TokensPerMonth: ptr(int64(1_000_000)), RPM: ptr(10)},
		"admins":   {Models: []string{"*"}},
	}
	for _, tc := range []struct {
		name        string
		memberships []string
		want        Policy
	}{
		{"none", nil, Policy{}},
		{"unknown", []string{"unknown"}, Policy{}},
		{"single", []string{"students"}, Policy{TokensPerDay: ptr(int64(300_000)), TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(20), MaxConcurrent: ptr(2)}},
		{"max per field", []string{"students", "staff"}, Policy{TokensPerDay: ptr(int64(2_000_000)), TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(60), MaxConcurrent: ptr(2)}},
		{"unknown does not uncap", []string{"unknown", "students"}, Policy{TokensPerDay: ptr(int64(300_000)), TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(20), MaxConcurrent: ptr(2)}},
		{"omitted fields uncap", []string{"students", "partial"}, Policy{TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(20)}},
		{"unlimited group", []string{"students", "admins", "staff"}, Policy{}},
		{"duplicates", []string{"students", "students"}, Policy{TokensPerDay: ptr(int64(300_000)), TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(20), MaxConcurrent: ptr(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 2 {
				got := Resolve(groups, tc.memberships)
				got.models = nil // model access is covered separately
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("limits for %v = %+v, want %+v", tc.memberships, got, tc.want)
				}
				slices.Reverse(tc.memberships)
			}
		})
	}
}

func TestResolvedPolicyDoesNotMutateConfig(t *testing.T) {
	g := config.Group{Models: []string{"chat"}, TokensPerDay: ptr(int64(10)), TokensPerMonth: ptr(int64(100)), RPM: ptr(5), MaxConcurrent: ptr(2)}
	groups := map[string]config.Group{"one": g, "two": g}
	for _, memberships := range [][]string{{"one"}, {"one", "two"}} {
		p := Resolve(groups, memberships)
		*p.TokensPerDay, *p.TokensPerMonth, *p.RPM, *p.MaxConcurrent = 1, 1, 1, 1
		if *g.TokensPerDay != 10 || *g.TokensPerMonth != 100 || *g.RPM != 5 || *g.MaxConcurrent != 2 {
			t.Fatal("resolved limits share pointers with config")
		}
	}
}
