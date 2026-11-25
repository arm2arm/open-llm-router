package usage

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// IncludeUsage must decide as the encoding/json implementation it replaced
// did, and the rewritten body must read the same to a backend. Only valid
// JSON objects reach it on the forwarding path.
func FuzzIncludeUsageMatchesReference(f *testing.F) {
	for _, seed := range []string{
		`{}`, `{"model":"m"}`, `{"stream_options":null}`, `{"stream_options":{}}`, `{"stream_options":{"include_usage":true}}`,
		`{"stream_options":{"include_usage":false,"future":123456789012345678901234567890}}`, `{"stream_options":{"include_usage":null}}`,
		`{"stream_options":{"include_usage":"yes"}}`, `{"stream_options":[]}`, `{"stream_options":"bad"}`, `null`, `[]`,
		`{"Stream_Options":{"include_usage":false}}`, `{"stream_options":{"include_usage":true,"Include_Usage":false}}`,
		`{"stream_options":{"include_usage":true},"stream_options":{"include_usage":false}}`,
		`{"stream_options":{"include_usage":true,"include_usage":false}}`, ` { "x" : [ 1 , 2 ] , "x" : 3 } `,
		"{\"k\xff\":\"<v>&\",\"stream_options\":{\"\xfe\":1}}",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if !json.Valid(body) {
			return
		}
		got, asked, err := IncludeUsage(body)
		want, wantAsked, wantErr := includeUsageReference(body)
		if (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() {
			t.Fatalf("error %v, reference %v", err, wantErr)
		}
		if err != nil {
			return
		}
		decode := func(data []byte) any {
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				t.Fatalf("invalid output %q: %v", data, err)
			}
			return value
		}
		if asked != wantAsked || !reflect.DeepEqual(decode(got), decode(want)) {
			t.Fatalf("rewrote %s (%v), reference %s (%v)", got, asked, want, wantAsked)
		}
	})
}
