package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/arm2arm/open-llm-router/internal/jsonobject"
)

var (
	errResponsesControls  = errors.New("responses control fields must be unique and spelled in lower case")
	errResponsesItems     = errors.New("responses input items must be JSON objects")
	errResponsesBooleans  = errors.New("responses store and background must be booleans")
	errResponsesState     = errors.New("OLR supports stateless Responses: use store:false and background:false")
	errResponsesHistory   = errors.New("OLR supports stateless Responses: send the full input history instead of previous_response_id or conversation")
	errResponsesReference = errors.New("OLR supports stateless Responses: include complete input items instead of stored item references")
)

// Stateless Responses is the mode Open WebUI uses by default. Backend storage
// is scoped to a shared provider credential, not to an OLR user, and stored
// identifiers would also need replica affinity. Until that lifecycle is owned
// by the router, do not let callers read someone else's provider-side state or
// start background work outside the request's quota/concurrency lease.
func statelessResponses(body []byte) ([]byte, bool, error) {
	object, err := jsonobject.Parse(body)
	if err != nil {
		return nil, false, errResponsesItems
	}
	fields, err := responseFields(object, "store", "background", "previous_response_id", "conversation", "input")
	if err != nil {
		return nil, false, err
	}
	for _, key := range []string{"store", "background"} {
		if raw := fields[key]; raw != nil {
			var value bool
			if json.Unmarshal(raw, &value) != nil || bytes.Equal(raw, []byte("null")) {
				return nil, false, errResponsesBooleans
			}
			if value {
				return nil, false, errResponsesState
			}
		}
	}
	for _, key := range []string{"previous_response_id", "conversation"} {
		if raw := fields[key]; len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte(`""`)) {
			return nil, false, errResponsesHistory
		}
	}
	if input, present, _ := object.Unique("input"); present {
		for item := range input.Elements() {
			entry, ok := item.Object()
			if !ok {
				return nil, false, errResponsesItems
			}
			if _, err := responseFields(entry, "type", "id", "content", "role"); err != nil {
				return nil, false, err
			}
			// Lower-casing also maps keys that case folding keeps apart,
			// such as "İd": treat those as present too.
			var kind string
			hasID, hasContent := false, false
			for member := range entry.Members() {
				switch strings.ToLower(member.Name()) {
				case "type":
					_ = json.Unmarshal(member.Value(), &kind)
				case "id":
					hasID = true
				case "content":
					hasContent = true
				}
			}
			if kind == "item_reference" || kind == "" && hasID && !hasContent {
				return nil, false, errResponsesReference
			}
		}
	}
	if fields["store"] != nil {
		return body, false, nil
	}
	return object.With(jsonobject.Field{Name: "store", Value: []byte("false")}), true, nil
}

// responseFields returns the values of the protected state-control keys. It
// refuses their case variants and duplicates before a backend decoder can
// interpret them differently. Unknown fields stay opaque.
func responseFields(object jsonobject.Object, protected ...string) (map[string][]byte, error) {
	fields := make(map[string][]byte, len(protected))
	for member := range object.Members() {
		for _, name := range protected {
			if member.Folds(name) {
				if _, seen := fields[name]; seen || !member.Is(name) {
					return nil, errResponsesControls
				}
				fields[name] = member.Value()
			}
		}
	}
	return fields, nil
}
