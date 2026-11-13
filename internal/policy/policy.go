// Package policy combines the configured policies of a caller's groups.
package policy

import (
	"slices"

	"github.com/arm2arm/open-llm-router/internal/config"
)

// Policy is the union of model access and the most permissive limits across
// matching groups. A nil limit is unlimited. The zero value grants no models.
// Limits are resolved here for quota/rate enforcement; this package does not
// enforce them.
type Policy struct {
	models             map[string]bool
	TokensPerDay       *int64
	TokensPerMonth     *int64
	RPM                *int
	TPM                *int64
	MaxConcurrent      *int // interactive keys
	MaxConcurrentBatch *int // batch keys
	StrictQuota        bool
	Admin              bool // a member of a group with admin: true
	Priority           int  // added to the key class's priority; lowest across groups
}

// Resolve ignores memberships absent from groups. Membership names match
// exactly, including case and any identity-provider path prefix.
func Resolve(groups map[string]config.Group, memberships []string) Policy {
	p := Policy{models: make(map[string]bool)}
	matched := false
	for _, name := range memberships {
		g, ok := groups[name]
		if !ok {
			continue
		}
		for _, model := range g.Models {
			p.models[model] = true
		}
		p.Admin = p.Admin || g.Admin
		batch := g.MaxConcurrentBatch
		if batch == nil {
			batch = g.MaxConcurrent
		}
		if !matched {
			p.TokensPerDay = clone(g.TokensPerDay)
			p.TokensPerMonth = clone(g.TokensPerMonth)
			p.RPM = clone(g.RPM)
			p.TPM = clone(g.TPM)
			p.MaxConcurrent = clone(g.MaxConcurrent)
			p.MaxConcurrentBatch = clone(batch)
			p.StrictQuota = g.StrictQuota
			p.Priority = g.Priority
			matched = true
			continue
		}
		p.TokensPerDay = mostPermissive(p.TokensPerDay, g.TokensPerDay)
		p.TokensPerMonth = mostPermissive(p.TokensPerMonth, g.TokensPerMonth)
		p.RPM = mostPermissive(p.RPM, g.RPM)
		p.TPM = mostPermissive(p.TPM, g.TPM)
		p.MaxConcurrent = mostPermissive(p.MaxConcurrent, g.MaxConcurrent)
		p.MaxConcurrentBatch = mostPermissive(p.MaxConcurrentBatch, batch)
		p.StrictQuota = p.StrictQuota && g.StrictQuota
		p.Priority = min(p.Priority, g.Priority)
	}
	return p
}

// ForUser is shared by enforcement and previews. The reserved local
// administrator's access is independent of every editable policy and override.
func ForUser(groups map[string]config.Group, memberships []string, overrides map[string]*int64, protected bool) Policy {
	if protected {
		return Policy{models: map[string]bool{"*": true}, Admin: true}
	}
	return Resolve(groups, memberships).WithOverrides(overrides)
}

// Override fields, as stored in quota_overrides.field.
const (
	FieldTokensPerDay       = "tokens_per_day"
	FieldTokensPerMonth     = "tokens_per_month"
	FieldRPM                = "rpm"
	FieldTPM                = "tpm"
	FieldMaxConcurrent      = "max_concurrent"
	FieldMaxConcurrentBatch = "max_concurrent_batch"
	FieldStrictQuota        = "strict_quota" // 1 on, 0 off
)

// OverrideFields lists every field a per-user override can set.
var OverrideFields = []string{FieldTokensPerDay, FieldTokensPerMonth, FieldRPM, FieldTPM,
	FieldMaxConcurrent, FieldMaxConcurrentBatch, FieldStrictQuota}

// ValidOverride reports whether field is known and value (nil for unlimited)
// is allowed for it.
func ValidOverride(field string, value *int64) bool {
	if !slices.Contains(OverrideFields, field) {
		return false
	}
	if field == FieldStrictQuota {
		return value != nil && (*value == 0 || *value == 1)
	}
	return value == nil || (*value > 0 && (field == FieldTokensPerDay || field == FieldTokensPerMonth || field == FieldTPM || *value <= 1<<31-1))
}

// WithOverrides replaces the group-derived limits with a user's overrides.
// A nil value means unlimited. Model access is never overridden here.
func (p Policy) WithOverrides(overrides map[string]*int64) Policy {
	for field, value := range overrides {
		if !ValidOverride(field, value) {
			continue
		}
		switch field {
		case FieldTokensPerDay:
			p.TokensPerDay = clone(value)
		case FieldTokensPerMonth:
			p.TokensPerMonth = clone(value)
		case FieldRPM:
			p.RPM = toInt(value)
		case FieldTPM:
			p.TPM = clone(value)
		case FieldMaxConcurrent:
			p.MaxConcurrent = toInt(value)
		case FieldMaxConcurrentBatch:
			p.MaxConcurrentBatch = toInt(value)
		case FieldStrictQuota:
			p.StrictQuota = *value == 1
		}
	}
	return p
}

// AllowsModel checks a canonical configured model name, after alias resolution
// and before replacing it with an upstream model name.
func (p Policy) AllowsModel(name string) bool {
	return p.models["*"] || p.models[name]
}

// Concurrency is the concurrency limit for a key class.
func (p Policy) Concurrency(class string) *int {
	if class == "batch" {
		return p.MaxConcurrentBatch
	}
	return p.MaxConcurrent
}

func toInt(v *int64) *int {
	if v == nil {
		return nil
	}
	value := int(*v)
	return &value
}

func clone[T any](p *T) *T {
	if p == nil {
		return nil
	}
	value := *p
	return &value
}

func mostPermissive[T ~int | ~int64](a, b *T) *T {
	if a == nil || b == nil {
		return nil
	}
	value := max(*a, *b)
	return &value
}
