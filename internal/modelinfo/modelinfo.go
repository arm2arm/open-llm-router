// Package modelinfo describes a model the way clients see it: what
// GET /v1/models, the portal's model list and the integration API report about
// each model or alias, so that agents and portals can set themselves up from it.
package modelinfo

import (
	"slices"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/pool"
)

// OwnedBy is the owned_by value of every GET /v1/models entry.
const OwnedBy = "open-llm-router"

// Metadata is what a client needs to know to use a model.
type Metadata struct {
	// ContextLength is the model's configured context_length or, without
	// one, the smallest context length its replicas' model catalogs report.
	// It is omitted while unknown.
	ContextLength int `json:"context_length,omitempty"`
	// Type is "chat" or "embedding".
	Type string `json:"type"`
	// Protocols are the APIs the model is served with: "openai", "anthropic"
	// or both.
	Protocols []string `json:"protocols"`
}

// Describe returns the metadata of name, a model or an alias, and the model it
// resolves to. An alias reports its target's values. pools are the replicas
// of the same routing snapshot as cfg; nil reports no detected context length.
// ok is false when name is neither a model nor an alias of one. Describe does
// not check access: callers authorize canonical first.
func Describe(cfg *config.Config, pools *pool.Manager, name string) (canonical string, metadata Metadata, ok bool) {
	canonical = name
	if target, alias := cfg.Aliases[name]; alias {
		canonical = target
	}
	model, ok := cfg.Models[canonical]
	if !ok {
		return canonical, Metadata{}, false
	}
	metadata = Metadata{Type: model.ModelType(), Protocols: slices.Clone(model.Protocols)}
	if metadata.Protocols == nil {
		metadata.Protocols = []string{}
	}
	if model.ContextLength != nil {
		metadata.ContextLength = *model.ContextLength
	} else {
		metadata.ContextLength = pools.ContextLength(canonical)
	}
	return canonical, metadata, true
}

// Entry is one entry of GET /v1/models: OpenAI's model object with the
// router's model metadata and recipient disclosure.
type Entry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	Metadata
	RecipientDetails   []policy.Recipient `json:"recipient_details"`
	RecipientsComplete bool               `json:"recipients_complete"`
}

// NewEntry builds the GET /v1/models entry of name, a model or an alias, for a
// caller in groups. Recipients follow the caller's backend access, as
// policy.ModelRecipients does. Callers authorize the canonical model first
// (policy.ModelAllowed); ok is false when name is unknown.
func NewEntry(cfg *config.Config, pools *pool.Manager, name string, groups []string, protectedAdmin bool) (entry Entry, ok bool) {
	canonical, metadata, ok := Describe(cfg, pools, name)
	if !ok {
		return Entry{}, false
	}
	recipients, complete := policy.ModelRecipients(cfg, canonical, groups, protectedAdmin)
	return Entry{ID: name, Object: "model", OwnedBy: OwnedBy, Metadata: metadata, RecipientDetails: recipients, RecipientsComplete: complete}, true
}
