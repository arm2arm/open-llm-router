package policy

import (
	"slices"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func BackendAllowed(cfg *config.Config, id string, groups []string, protected bool) bool {
	if id == "" || protected {
		return true
	}
	backend, ok := cfg.Backends[id]
	if !ok {
		return false
	}
	if backend.Groups == nil {
		return true
	}
	for _, group := range groups {
		if cfg.Groups[group].Admin || slices.Contains(backend.Groups, group) {
			return true
		}
	}
	return false
}

func ModelAllowed(cfg *config.Config, access Policy, name string, groups []string, protected bool) bool {
	if !access.Admin && !access.AllowsModel(name) {
		return false
	}
	model, exists := cfg.Models[name]
	if !exists {
		return false
	}
	if len(model.Pool) == 0 {
		return true
	}
	for _, replica := range model.Pool {
		if BackendAllowed(cfg, replica.Backend, groups, protected) {
			return true
		}
	}
	return false
}
