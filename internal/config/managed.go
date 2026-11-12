package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WithModels validates an immutable routing copy against the file-owned group
// and alias policies. Resolved tokens are reconstructed, never accepted in JSON.
func WithModels(base *Config, models map[string]Model, lookup func(string) (string, bool)) (*Config, error) {
	data, err := json.Marshal(models)
	if err != nil {
		return nil, Problems{"invalid model configuration"}
	}
	var copied map[string]Model
	if json.Unmarshal(data, &copied) != nil {
		return nil, Problems{"invalid model configuration"}
	}
	cfg := *base
	cfg.Models = copied
	var problems Problems
	for name, model := range cfg.Models {
		if model.IdleTimeoutS == 0 {
			model.IdleTimeoutS = 30
		}
		for i := range model.Pool {
			r := &model.Pool[i]
			if !isBackendURL(r.URL) {
				problems = append(problems, fmt.Sprintf("models.%s.pool[%d]: backend URL must be an HTTP(S) origin without credentials, path, query or fragment", name, i))
				continue
			}
			r.URL = strings.TrimRight(r.URL, "/")
			if r.BearerTokenEnv != "" {
				token, ok := lookup(r.BearerTokenEnv)
				if !isVarName(r.BearerTokenEnv) || !ok || token == "" || strings.ContainsAny(token, "\r\n") {
					problems = append(problems, fmt.Sprintf("models.%s.pool[%d]: choose an available backend credential reference", name, i))
				} else {
					r.BearerToken = token
				}
			}
		}
		cfg.Models[name] = model
	}
	if len(problems) > 0 {
		return nil, problems
	}
	if problems = cfg.validate(); len(problems) > 0 {
		return nil, problems
	}
	return &cfg, nil
}
