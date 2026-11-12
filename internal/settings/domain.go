package settings

import (
	"maps"
	"slices"

	"github.com/arm2arm/open-llm-router/internal/config"
)

// document is one settings domain's document type, implemented by a pointer
// to a plain struct. No method returns a secret value.
type document[D any] interface {
	*D
	domainName() string
	// decode reads a document strictly and fills defaults.
	decode([]byte) error
	// baseline sets the document to the current effective configuration,
	// the import source.
	baseline(current *config.Config)
	// problems reports the document's own problems, without resolving secrets.
	problems() []string
	// secrets lists the secret settings with references into the document.
	secrets() []secretBinding
	// apply builds the runtime configuration from base and resolved secrets.
	apply(base *config.Config, in applyInput) (*config.Config, error)
	// conflicts compares file/environment values with the document.
	conflicts(file *config.Config, secrets map[string]string, known bool) []Conflict
	// conflictsBlock reports whether conflicts refuse activation.
	conflictsBlock() bool
	// values returns comparable non-secret values by setting name.
	values() map[string]string
	// settingNames lists the settings in display order.
	settingNames() []string
	// fileSources reports each setting's source while file-managed.
	fileSources(file *config.Config) map[string]string
	// checksPassed reports whether stored checks allow Admin activation.
	checksPassed(*Checks) bool
	// fallback is the configuration to start with when the active revision
	// cannot be applied, with the message to log; ok false fails startup.
	fallback(base *config.Config) (cfg *config.Config, message string, ok bool)
}

// secretBinding is one secret setting: the only environment variable it may
// reference, and its reference inside a document.
type secretBinding struct {
	name, env string
	ref       *SecretRef
}

type applyInput struct {
	secrets map[string]string           // resolved secret values by setting name
	tokens  func(string) (string, bool) // backend credential references
}

func bindingNamed(bindings []secretBinding, name string) (secretBinding, bool) {
	for _, binding := range bindings {
		if binding.name == name {
			return binding, true
		}
	}
	return secretBinding{}, false
}

// changed lists the settings whose values or secret references differ.
func changed[D any, P document[D]](before, after P) []string {
	encode := func(doc P) map[string]string {
		values := doc.values()
		for _, binding := range doc.secrets() {
			values[binding.name] = binding.ref.Env + "|" + binding.ref.Stored
		}
		return values
	}
	old, next := encode(before), encode(after)
	keys := map[string]bool{}
	for key := range old {
		keys[key] = true
	}
	for key := range next {
		keys[key] = true
	}
	names := []string{}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		if old[key] != next[key] {
			names = append(names, key)
		}
	}
	return names
}
