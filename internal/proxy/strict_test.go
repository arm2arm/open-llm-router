package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestStrictQuotaRejectsUnboundedShapesAndNormalizesLimits(t *testing.T) {
	for _, fields := range []string{
		`"n":2`, `"best_of":3`, `"n":"1"`, `"n":null`, `"n":1.00000000000000000001`,
		`"prompt":["one","two"]`, `"prompt":[1,2]`, `"prompt":{"id":"remote-template"}`,
		`"max_tokens":"20"`, `"max_tokens":null`, `"max_tokens":0`, `"max_tokens":-1`, `"max_tokens":1.2`, `"max_tokens":1e10000`,
		`"max_completion_tokens":"20"`, `"max_output_tokens":false`,
		`"messages":[{"content":[{"type":"image_url","image_url":{"url":"https://media.example/image.png"}}]}]`,
		`"input":[{"type":"input_file","file_id":"remote"}]`,
		`"messages":[{"content":[{"type":"image","source":{"type":"url","url":"https://media.example/image.png"}}]}]`,
		`"messages":[{"content":[{"type":"input_audio","input_audio":{"data":"remote"}}]}]`,
		`"messages":[{"content":[],"content":"hidden-first-value"}]`,
		`"tools":[{"type":"web_search_preview"}]`,
		`"modalities":["audio"]`, `"audio":{"format":"wav"}`,
		`"Messages":[{"content":[{"type":"image"}]}]`,
		`"messages":[{"content":[{"Type":"image","source":{"url":"https://media.example/image"}}]}]`,
		`"tools":[{"Type":"web_search"}]`, `"tools":["web_search"]`,
		`"modalities":"audio"`, `"tools":{"type":"web_search"}`,
	} {
		body := []byte(`{"model":"m","max_tokens":20,` + fields + `}`)
		// Cases that replace max_tokens avoid testing only a duplicate key.
		if strings.HasPrefix(fields, `"max_tokens"`) {
			body = []byte(`{"model":"m",` + fields + `}`)
		}
		meta, err := parseRequest(body)
		if err != nil {
			t.Fatal("invalid test input", err)
		}
		if _, err := strictQuotaRequest(body, "/v1/chat/completions", meta); err == nil {
			t.Errorf("strict quota accepted %s", fields)
		}
	}
	body := []byte(`{"model":"m","max_tokens":10,"max_completion_tokens":20,"n":1,"best_of":1,"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"seed":{"const":1234567890123456789}}}}}]}`)
	meta, err := parseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	forwarded, err := strictQuotaRequest(body, "/v1/chat/completions", meta)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(forwarded, &fields); err != nil || fields["max_tokens"] != nil || string(fields["max_completion_tokens"]) != "20" || !bytes.Contains(forwarded, []byte("1234567890123456789")) {
		t.Fatal("forwarded output limits differ from reservation or integer lost precision", err)
	}
	if _, err := strictQuotaRequest([]byte(`{"model":"m","max_tokens":1,"messages":`+strings.Repeat("[", 100)+`0`+strings.Repeat("]", 100)+`}`), "/v1/chat/completions", requestMeta{MaxTokens: 1}); err == nil {
		t.Fatal("unbounded nesting accepted")
	}
}

func TestQuotaControlsAreUnambiguous(t *testing.T) {
	for _, body := range []string{`{"model":"m","n":1,"n":2}`, `{"model":"m","N":2}`, `{"model":"m","best_of":1,"BEST_OF":3}`, `{"model":"m","prompt":"one","prompt":["two","three"]}`} {
		if _, err := parseRequest([]byte(body)); err != errAmbiguousField {
			t.Fatal("ambiguous work multiplier accepted", body, err)
		}
	}
}
