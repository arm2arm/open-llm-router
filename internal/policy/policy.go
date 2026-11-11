// Package policy combines the configured policies of a caller's groups.
package policy

import "github.com/arm2arm/open-llm-router/internal/config"

// Policy is the union of model access and the most permissive limits across
// matching groups. A nil limit is unlimited. The zero value grants no models.
// Limits are resolved here for quota/rate enforcement; this package does not
// enforce them.
type Policy struct {
	models         map[string]bool
	TokensPerDay   *int64
	TokensPerMonth *int64
	RPM            *int
	MaxConcurrent  *int
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
		if !matched {
			p.TokensPerDay = clone(g.TokensPerDay)
			p.TokensPerMonth = clone(g.TokensPerMonth)
			p.RPM = clone(g.RPM)
			p.MaxConcurrent = clone(g.MaxConcurrent)
			matched = true
			continue
		}
		p.TokensPerDay = mostPermissive(p.TokensPerDay, g.TokensPerDay)
		p.TokensPerMonth = mostPermissive(p.TokensPerMonth, g.TokensPerMonth)
		p.RPM = mostPermissive(p.RPM, g.RPM)
		p.MaxConcurrent = mostPermissive(p.MaxConcurrent, g.MaxConcurrent)
	}
	return p
}

// AllowsModel checks a canonical configured model name, after alias resolution
// and before replacing it with an upstream model name.
func (p Policy) AllowsModel(name string) bool {
	return p.models["*"] || p.models[name]
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
