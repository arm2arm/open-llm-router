package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
)

// These are the encoding/json implementations that jsonobject replaced. The
// differential fuzz tests require identical decisions on every input.

func parseRequestReference(body []byte) (requestMeta, error) {
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
		case "max_tokens", "max_completion_tokens", "max_output_tokens":
			// Lenient: the backend validates the value; only a usable
			// number sizes a reservation.
			var raw json.RawMessage
			err = decoder.Decode(&raw)
			var limit float64
			if err == nil && json.Unmarshal(raw, &limit) == nil && limit > 0 && limit < 1<<53 {
				meta.MaxTokens = max(meta.MaxTokens, int64(math.Ceil(limit)))
				if field == "max_output_tokens" {
					meta.MaxOutputTokens = int64(math.Ceil(limit))
				}
			}
		default:
			var skip json.RawMessage
			err = decoder.Decode(&skip)
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

func statelessResponsesReference(body []byte) ([]byte, bool, error) {
	fields, err := responseFieldsReference(body, []string{"store", "background", "previous_response_id", "conversation", "input"})
	if err != nil {
		return nil, false, err
	}
	for _, key := range []string{"store", "background"} {
		if raw := fields[key]; raw != nil {
			var value bool
			if json.Unmarshal(raw, &value) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return nil, false, errors.New("responses store and background must be booleans")
			}
			if value {
				return nil, false, errors.New("OLR supports stateless Responses: use store:false and background:false")
			}
		}
	}
	for _, key := range []string{"previous_response_id", "conversation"} {
		if raw := bytes.TrimSpace(fields[key]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte(`""`)) {
			return nil, false, errors.New("OLR supports stateless Responses: send the full input history instead of previous_response_id or conversation")
		}
	}
	var items []json.RawMessage
	if json.Unmarshal(fields["input"], &items) == nil {
		for _, raw := range items {
			item, err := responseFieldsReference(raw, []string{"type", "id", "content", "role"})
			if err != nil {
				return nil, false, err
			}
			var kind string
			hasID, hasContent := false, false
			for key, raw := range item {
				switch strings.ToLower(key) {
				case "type":
					_ = json.Unmarshal(raw, &kind)
				case "id":
					hasID = true
				case "content":
					hasContent = true
				}
			}
			if kind == "item_reference" || kind == "" && hasID && !hasContent {
				return nil, false, errors.New("OLR supports stateless Responses: include complete input items instead of stored item references")
			}
		}
	}
	if fields["store"] != nil {
		return body, false, nil
	}
	fields["store"] = json.RawMessage("false")
	out, err := json.Marshal(fields)
	return out, true, err
}

func responseFieldsReference(body []byte, protected []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("responses input items must be JSON objects")
	}
	fields := map[string]json.RawMessage{}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		for _, name := range protected {
			if strings.EqualFold(key, name) {
				if key != name || seen[name] {
					return nil, errors.New("responses control fields must be unique and spelled in lower case")
				}
				seen[name] = true
			}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("invalid Responses object")
	}
	return fields, nil
}
