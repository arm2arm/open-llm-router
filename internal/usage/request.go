package usage

import (
	"encoding/json"
	"errors"
)

// IncludeUsage requests accounting metadata while preserving unknown fields and
// numeric values. asked reports whether the client wanted the extra chunk.
func IncludeUsage(body []byte) (updated []byte, asked bool, err error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, false, errors.New("invalid request")
	}
	var options map[string]json.RawMessage
	if raw := fields["stream_options"]; raw != nil {
		if json.Unmarshal(raw, &options) != nil {
			return nil, false, errors.New("invalid stream_options")
		}
	}
	if options == nil {
		options = make(map[string]json.RawMessage)
	}
	if raw := options["include_usage"]; raw != nil {
		if json.Unmarshal(raw, &asked) != nil {
			return nil, false, errors.New("invalid include_usage")
		}
	}
	if asked {
		return body, true, nil
	}
	options["include_usage"] = json.RawMessage("true")
	fields["stream_options"], _ = json.Marshal(options)
	updated, err = json.Marshal(fields)
	return updated, false, err
}
