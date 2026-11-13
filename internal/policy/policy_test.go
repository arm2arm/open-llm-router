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
		{"single", []string{"students"}, Policy{TokensPerDay: ptr(int64(300_000)), TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(20), MaxConcurrent: ptr(2), MaxConcurrentBatch: ptr(2)}},
		{"max per field", []string{"students", "staff"}, Policy{TokensPerDay: ptr(int64(2_000_000)), TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(60), MaxConcurrent: ptr(2), MaxConcurrentBatch: ptr(2)}},
		{"unknown does not uncap", []string{"unknown", "students"}, Policy{TokensPerDay: ptr(int64(300_000)), TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(20), MaxConcurrent: ptr(2), MaxConcurrentBatch: ptr(2)}},
		{"omitted fields uncap", []string{"students", "partial"}, Policy{TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(20)}},
		{"unlimited group", []string{"students", "admins", "staff"}, Policy{}},
		{"duplicates", []string{"students", "students"}, Policy{TokensPerDay: ptr(int64(300_000)), TokensPerMonth: ptr(int64(9_000_000)), RPM: ptr(20), MaxConcurrent: ptr(2), MaxConcurrentBatch: ptr(2)}},
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

func TestBatchConcurrencyStrictQuotaAndTPM(t *testing.T) {
	groups := map[string]config.Group{
		"a": {Models: []string{"chat"}, MaxConcurrent: ptr(2), MaxConcurrentBatch: ptr(8), TPM: ptr(int64(1000)), StrictQuota: true},
		"b": {Models: []string{"chat"}, MaxConcurrent: ptr(4), TPM: ptr(int64(5000)), StrictQuota: true},
		"c": {Models: []string{"chat"}, MaxConcurrent: ptr(1), TPM: ptr(int64(10))},
	}
	p := Resolve(groups, []string{"a"})
	if *p.Concurrency("interactive") != 2 || *p.Concurrency("batch") != 8 || *p.TPM != 1000 || !p.StrictQuota {
		t.Fatalf("a: %+v", p)
	}
	p = Resolve(groups, []string{"a", "b"})
	if *p.Concurrency("interactive") != 4 || *p.Concurrency("batch") != 8 || *p.TPM != 5000 || !p.StrictQuota {
		t.Fatalf("a+b: %+v", p)
	}
	// Strict only if every matching group is strict (most permissive wins).
	if Resolve(groups, []string{"a", "c"}).StrictQuota {
		t.Fatal("a non-strict group must lift strict mode")
	}
}

func TestOverrides(t *testing.T) {
	groups := map[string]config.Group{"students": {Models: []string{"chat"}, TokensPerDay: ptr(int64(100)), RPM: ptr(2), MaxConcurrent: ptr(1)}}
	base := Resolve(groups, []string{"students"})
	p := base.WithOverrides(map[string]*int64{
		FieldTokensPerDay: nil, FieldRPM: ptr(int64(50)), FieldTPM: ptr(int64(900)),
		FieldMaxConcurrentBatch: ptr(int64(6)), FieldStrictQuota: ptr(int64(1)),
		"unknown": ptr(int64(1)), FieldMaxConcurrent: ptr(int64(0)), // invalid values are ignored
	})
	if p.TokensPerDay != nil || *p.RPM != 50 || *p.TPM != 900 || *p.MaxConcurrentBatch != 6 || !p.StrictQuota || *p.MaxConcurrent != 1 || !p.AllowsModel("chat") {
		t.Fatalf("overridden: %+v", p)
	}
	if *base.TokensPerDay != 100 || *base.RPM != 2 {
		t.Fatal("overrides changed the base policy")
	}
	for field, value := range map[string]*int64{FieldStrictQuota: nil, FieldRPM: ptr(int64(-1)), FieldMaxConcurrent: ptr(int64(1 << 40)), "groups": nil} {
		if ValidOverride(field, value) {
			t.Errorf("%s accepted", field)
		}
	}
}

func TestProtectedAdministratorIgnoresEditablePolicies(t *testing.T) {
	one := int64(1)
	p := ForUser(map[string]config.Group{"restricted": {Models: []string{"only"}}}, []string{"restricted"}, map[string]*int64{FieldRPM: &one, FieldStrictQuota: &one}, true)
	if !p.Admin || !p.AllowsModel("anything") || p.RPM != nil || p.StrictQuota || p.Priority != 0 {
		t.Fatal("protected admin restricted", p)
	}
}
