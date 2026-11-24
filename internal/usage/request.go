package usage

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/arm2arm/open-llm-router/internal/jsonobject"
)

// IncludeUsage requests accounting metadata while preserving unknown fields and
// numeric values. asked reports whether the client wanted the extra chunk.
//
// Duplicate and case-variant spellings of the keys the router changes are
// refused: a Go-struct backend accepts case variants and uses the last match,
// other backends may use the first. Neither interpretation may override the
// router's usage request. Only stream_options is rewritten; every other
// member keeps its bytes.
func IncludeUsage(body []byte) (updated []byte, asked bool, err error) {
	object, err := jsonobject.Parse(body)
	if err != nil {
		return nil, false, errors.New("invalid request")
	}
	options, present, err := object.Unique("stream_options")
	if err != nil {
		return nil, false, errors.New("invalid request")
	}
	value := []byte(`{"include_usage":true}`)
	if present && !bytes.Equal(options.Value(), []byte("null")) {
		nested, ok := options.Object()
		if !ok {
			return nil, false, errors.New("invalid stream_options")
		}
		include, present, err := nested.Unique("include_usage")
		if err != nil {
			return nil, false, errors.New("invalid stream_options")
		}
		if present && json.Unmarshal(include.Value(), &asked) != nil {
			return nil, false, errors.New("invalid include_usage")
		}
		if asked {
			return body, true, nil
		}
		value = nested.With(jsonobject.Field{Name: "include_usage", Value: []byte("true")})
	}
	return object.With(jsonobject.Field{Name: "stream_options", Value: value}), false, nil
}
