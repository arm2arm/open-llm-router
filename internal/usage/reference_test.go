package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// The encoding/json implementation that jsonobject replaced. The differential
// fuzz test requires identical decisions and equivalent output on every input.

func includeUsageReference(body []byte) (updated []byte, asked bool, err error) {
	fields, err := accountingObjectReference(body, "stream_options")
	if err != nil || fields == nil {
		return nil, false, errors.New("invalid request")
	}
	var options map[string]json.RawMessage
	if raw := fields["stream_options"]; raw != nil {
		options, err = accountingObjectReference(raw, "include_usage")
		if err != nil {
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

func accountingObjectReference(raw []byte, protected string) (map[string]json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid accounting options")
	}
	fields := map[string]json.RawMessage{}
	seen := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		if strings.EqualFold(key, protected) {
			if key != protected || seen {
				return nil, errors.New("ambiguous accounting option")
			}
			seen = true
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
		return nil, errors.New("trailing accounting options")
	}
	return fields, nil
}
