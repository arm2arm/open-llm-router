package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// routingFields are the top-level request fields the router acts on. Their
// spelling must be unambiguous: encoding/json matches struct fields without
// regard to case and keeps the last duplicate, while a backend may read the
// exact key or the first duplicate. A body such as
// {"model":"secret","MODEL":"allowed"} would otherwise be authorized as one
// model and served as another by a backend that serves both.
var routingFields = []string{"model", "stream"}

var errAmbiguousField = errors.New("ambiguous routing field")

type requestMeta struct {
	Model  string
	Stream bool
}

// parseRequest reads the routing fields from a JSON object in one pass. It
// refuses a top-level key that matches a routing field only when case is
// ignored, and any duplicate of a routing field.
func parseRequest(body []byte) (requestMeta, error) {
	var meta requestMeta
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return meta, errors.New("not a JSON object")
	}
	seen := make(map[string]bool, len(routingFields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return meta, err
		}
		key, _ := token.(string)
		field := ""
		for _, name := range routingFields {
			if strings.EqualFold(key, name) {
				field = name
			}
		}
		if field == "" {
			var skip json.RawMessage
			if err := decoder.Decode(&skip); err != nil {
				return meta, err
			}
			continue
		}
		if key != field || seen[field] {
			return meta, errAmbiguousField
		}
		seen[field] = true
		switch field {
		case "model":
			err = decoder.Decode(&meta.Model)
		case "stream":
			var stream *bool
			err = decoder.Decode(&stream)
			meta.Stream = stream != nil && *stream
		}
		if err != nil {
			return meta, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return meta, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return meta, errors.New("trailing data after the JSON object")
	}
	return meta, nil
}
