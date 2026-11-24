package proxy

import (
	"encoding/json"
	"errors"
	"math"

	"github.com/arm2arm/open-llm-router/internal/jsonobject"
)

// routingFields are the top-level request fields the router acts on. Their
// spelling must be unambiguous: encoding/json matches struct fields without
// regard to case and keeps the last duplicate, while a backend may read the
// exact key or the first duplicate. A body such as
// {"model":"secret","MODEL":"allowed"} would otherwise be authorized as one
// model and served as another by a backend that serves both.
// Output-token limits size strict-quota reservations.
var routingFields = [...]string{"model", "stream", "max_tokens", "max_completion_tokens", "max_output_tokens", "n", "best_of", "prompt"}

var errAmbiguousField = errors.New("ambiguous routing field")

type requestMeta struct {
	Model  string
	Stream bool
	// MaxTokens is the largest of max_tokens, max_completion_tokens and
	// max_output_tokens; 0 if none is set.
	MaxTokens       int64
	MaxOutputTokens int64
}

// parseRequest reads the routing fields of a JSON object. It refuses a
// top-level key that matches a routing field only when case is ignored, and
// any duplicate of a routing field. Other values are validated, not copied.
func parseRequest(body []byte) (requestMeta, error) {
	var meta requestMeta
	object, err := jsonobject.Parse(body)
	if err != nil {
		return meta, err
	}
	var seen [len(routingFields)]bool
	for member := range object.Members() {
		field := -1
		for i, name := range routingFields {
			if member.Folds(name) {
				field = i
			}
		}
		if field < 0 {
			continue
		}
		if !member.Is(routingFields[field]) || seen[field] {
			return meta, errAmbiguousField
		}
		seen[field] = true
		value := member.Value()
		switch routingFields[field] {
		case "model":
			err = json.Unmarshal(value, &meta.Model)
		case "stream":
			var stream *bool
			err = json.Unmarshal(value, &stream)
			meta.Stream = stream != nil && *stream
		case "max_tokens", "max_completion_tokens", "max_output_tokens":
			// Lenient: the backend validates the value; only a usable
			// number sizes a reservation.
			var limit float64
			if json.Unmarshal(value, &limit) == nil && limit > 0 && limit < 1<<53 {
				meta.MaxTokens = max(meta.MaxTokens, int64(math.Ceil(limit)))
				if routingFields[field] == "max_output_tokens" {
					meta.MaxOutputTokens = int64(math.Ceil(limit))
				}
			}
		}
		if err != nil {
			return meta, err
		}
	}
	return meta, nil
}
