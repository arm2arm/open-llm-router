package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/jsonobject"
)

var bodySeeds = []string{
	`{}`, `{"model":"chat-model","stream":true}`, `{"model":"one","MODEL":"two"}`, `{"model":null}`,
	`{"messages":[{"model":"nested"}],"model":"canonical"}`, `{"model":"雪","max_tokens":3.5}`,
	` {"model" : "a" , "n" : 12345678901234567890 } `, `{"model":"a","max_tokens":"x","max_output_tokens":7}`,
	`{"model":"a","stream":"yes"}`, `{"model":1}`, `{"model":"a"} {}`, `{"model":"a",}`, `[]`, `"x"`, ``,
	`{"model":"m","store":false,"STORE":true}`, `{"model":"m","store":true}`, `{"model":"m","background":null}`,
	`{"model":"m","previous_response_id":""}`, `{"model":"m","conversation":{"id":"c"}}`,
	`{"model":"m","input":[{"type":"item_reference","id":"x"}]}`, `{"model":"m","input":[{"id":"x"}]}`,
	`{"model":"m","input":[{"İd":"x"}]}`, `{"model":"m","input":[{"role":"user","content":"x","id":"i"}]}`,
	`{"model":"m","input":"text","store":false}`, `{"model":"m","input":[1]}`, `{"model":"m","input":null}`,
	`{"model":"m","input":[{"type":"message","TYPE":"item_reference"}]}`, `{"model":"m","x":1,"x":2,"input":[]}`,
	"{\"model\":\"m\",\"k\xff\":\"v\",\"input\":[{\"content\":\"\xfe\"}]}",
}

// The jsonobject parser must make the decision of the encoding/json decoder it
// replaced for every body: success, the routed fields, and whether a refusal
// is an ambiguous spelling (which has its own client message).
func FuzzParseRequestMatchesReference(f *testing.F) {
	for _, seed := range bodySeeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		got, err := parseRequest(body)
		want, wantErr := parseRequestReference(body)
		if !json.Valid(body) {
			if err == nil || wantErr == nil {
				t.Fatalf("invalid JSON accepted: %v / %v", err, wantErr)
			}
			return
		}
		if (err == nil) != (wantErr == nil) || errors.Is(err, errAmbiguousField) != errors.Is(wantErr, errAmbiguousField) {
			t.Fatalf("error %v, reference %v", err, wantErr)
		}
		if err == nil && got != want {
			t.Fatalf("meta %+v, reference %+v", got, want)
		}
	})
}

// Responses errors reach clients verbatim, and the forwarded body must read
// the same to a backend. Only bodies that passed parseRequest arrive here.
func FuzzStatelessResponsesMatchesReference(f *testing.F) {
	for _, seed := range bodySeeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if _, err := parseRequest(body); err != nil {
			return
		}
		got, changed, err := statelessResponses(body)
		want, wantChanged, wantErr := statelessResponsesReference(body)
		if (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() {
			t.Fatalf("error %v, reference %v", err, wantErr)
		}
		if err != nil {
			return
		}
		if changed != wantChanged || !sameJSON(t, got, want) {
			t.Fatalf("forwarded %s (%v), reference %s (%v)", got, changed, want, wantChanged)
		}
	})
}

// sameJSON compares what encoding/json reads, which keeps the last duplicate
// as the reference's re-encoding did.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	decode := func(data []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("invalid output %q: %v", data, err)
		}
		return value
	}
	return reflect.DeepEqual(decode(a), decode(b))
}

func TestBodyParsersMatchReferenceOnSeeds(t *testing.T) {
	for _, seed := range bodySeeds {
		body := []byte(seed)
		got, err := parseRequest(body)
		want, wantErr := parseRequestReference(body)
		if (err == nil) != (wantErr == nil) || err == nil && got != want {
			t.Errorf("parseRequest(%q) = %+v, %v; reference %+v, %v", seed, got, err, want, wantErr)
		}
		if err != nil {
			continue
		}
		out, changed, err := statelessResponses(body)
		ref, refChanged, refErr := statelessResponsesReference(body)
		if (err == nil) != (refErr == nil) || err != nil && err.Error() != refErr.Error() || err == nil && (changed != refChanged || !sameJSON(t, out, ref)) {
			t.Errorf("statelessResponses(%q) = %s, %v, %v; reference %s, %v, %v", seed, out, changed, err, ref, refChanged, refErr)
		}
	}
}

func BenchmarkForwardedBodyRewrite(b *testing.B) {
	var messages []map[string]string
	for range 512 {
		messages = append(messages, map[string]string{"role": "user", "content": string(bytes.Repeat([]byte("a <tagged> & \"quoted\" word "), 37))})
	}
	body, _ := json.Marshal(map[string]any{"model": "chat-model", "stream": true, "max_tokens": 512, "messages": messages})
	b.Run("reference", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := parseRequestReference(body); err != nil {
				b.Fatal(err)
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(body, &fields)
			fields["model"], _ = json.Marshal("upstream-model")
			_, _ = json.Marshal(fields)
		}
	})
	b.Run("jsonobject", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := parseRequest(body); err != nil {
				b.Fatal(err)
			}
			object, _ := jsonobject.Parse(body)
			_ = object.With(jsonobject.Field{Name: "model", Value: []byte(`"upstream-model"`)})
		}
	})
}
