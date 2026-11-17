package proxy

import (
	"bytes"
	"encoding/json"
	"testing"
)

func FuzzRequestModelExtraction(f *testing.F) {
	for _, seed := range []string{`{}`, `{"model":"chat-model","stream":true}`, `{"model":"one","MODEL":"two"}`, `{"messages":[{"model":"nested"}],"model":"canonical"}`, `{"model":null}`, `{"model":"雪","max_tokens":3.5}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		meta, err := parseRequest(body)
		if err != nil {
			return
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(body, &object); err != nil || object == nil {
			t.Fatal("accepted a non-object or trailing JSON")
		}
		var model string
		if raw, exists := object["model"]; exists {
			if err := json.Unmarshal(raw, &model); err != nil {
				t.Fatal("accepted invalid model type")
			}
		}
		if meta.Model != model {
			t.Fatal("routed a different model than the canonical JSON field")
		}
		inside := bytes.TrimSpace(bytes.TrimSpace(body)[1 : len(bytes.TrimSpace(body))-1])
		inject := func(field string) []byte {
			prefix := []byte(`{"` + field + `":"fuzz-injected"`)
			if len(inside) != 0 {
				prefix = append(prefix, ',')
			}
			prefix = append(prefix, inside...)
			return append(prefix, '}')
		}
		if _, err := parseRequest(inject("MODEL")); err == nil {
			t.Fatal("accepted a case-ambiguous routing field")
		}
		if _, exists := object["model"]; exists {
			if _, err := parseRequest(inject("model")); err == nil {
				t.Fatal("accepted duplicate model fields")
			}
		}
	})
}
