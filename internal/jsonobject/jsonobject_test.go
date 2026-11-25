package jsonobject

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

type pair struct {
	name  string
	value string
}

// reference reads the members with encoding/json's token decoder, the
// implementation this package must agree with.
func reference(data []byte) ([]pair, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	var pairs []pair
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		pairs = append(pairs, pair{token.(string), string(bytes.TrimSpace(value))})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return pairs, nil
}

func members(o Object) []pair {
	var pairs []pair
	for m := range o.Members() {
		pairs = append(pairs, pair{m.Name(), string(m.Value())})
	}
	return pairs
}

func elements(m Member) []string {
	var values []string
	for e := range m.Elements() {
		values = append(values, string(e.Value()))
	}
	return values
}

func nth(o Object, n int) Member {
	for m := range o.Members() {
		if n == 0 {
			return m
		}
		n--
	}
	return Member{}
}

var seeds = []string{
	`{}`, ` { } `, "\t{\"a\":1}\n", `{"a":"b","c":[1,{"d":"}"}],"e":{"f":"]"}}`,
	`{"model":"chat","messages":[{"role":"user","content":"say \"hi\" \\"}],"stream":true}`,
	`{"a\"b":"c\\","d":"\\\"","e":"\\\\"}`, `{"model":"x","mödel":"y"}`,
	"{\"k\xff\":\"v\xfe\"}", `{"n":-1.5e+10,"t":true,"f":false,"z":null,"s":""}`,
	`{"a" : 1 , "b" :[ ] , "c" :{ } }`, `{"a":1,"a":2,"A":3}`, `{"deep":[[[[{"x":[["y"]]}]]]]}`,
	`[]`, `"x"`, `1`, `null`, ``, `{`, `{"a":1,}`, `{"a":1} {}`, `{"a":1}x`, `{"a":01}`, "{\"a\":\"\x01\"}",
}

func FuzzParseMatchesEncodingJSON(f *testing.F) {
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		want, wantErr := reference(data)
		object, err := Parse(data)
		if (err != nil) != (wantErr != nil) {
			t.Fatalf("Parse error %v, encoding/json error %v", err, wantErr)
		}
		if err == nil && !slices.Equal(members(object), want) {
			t.Fatalf("members %q, want %q", members(object), want)
		}
	})
}

func TestParse(t *testing.T) {
	for _, seed := range seeds {
		want, wantErr := reference([]byte(seed))
		object, err := Parse([]byte(seed))
		if (err != nil) != (wantErr != nil) || err == nil && !slices.Equal(members(object), want) {
			t.Errorf("%q: %v %v, want %q %v", seed, object, err, want, wantErr)
		}
	}
	if _, err := Parse([]byte(`[1]`)); err != ErrNotObject {
		t.Fatal("array accepted as object", err)
	}
	object, _ := Parse([]byte(`{"o":{"a":[1,"]",{"b":2}],"c":"d"},"l":[{"x":1}, "y" ,[]],"s":"t","e":[ ]}`))
	nested, ok := nth(object, 0).Object()
	if !ok || !slices.Equal(members(nested), []pair{{"a", `[1,"]",{"b":2}]`}, {"c", `"d"`}}) {
		t.Fatal("nested object", members(nested))
	}
	if got := elements(nth(object, 1)); !slices.Equal(got, []string{`{"x":1}`, `"y"`, `[]`}) {
		t.Fatal("elements", got)
	}
	if _, ok := nth(object, 2).Object(); ok {
		t.Fatal("string parsed as object")
	}
	if got := elements(nth(object, 2)); got != nil {
		t.Fatal("string iterated as array", got)
	}
	if got := elements(nth(object, 3)); got != nil {
		t.Fatal("empty array has elements", got)
	}
	if _, ok := (Member{}).Object(); ok || (Member{}).Name() != "" || (Member{}).Is("") || (Member{}).Folds("") {
		t.Fatal("zero member has content")
	}
	escaped, _ := Parse([]byte(`{"\u006dodel":1,"Mod\u0065L":2,"\u00e9":3}`))
	if m := nth(escaped, 0); !m.Is("model") || m.Is("Model") || !m.Folds("MODEL") || m.Name() != "model" {
		t.Fatal("escaped name", m.Name())
	}
	if m := nth(escaped, 1); m.Is("model") || !m.Folds("model") {
		t.Fatal("escaped case variant", m.Name())
	}
	if m := nth(escaped, 2); !m.Is("é") || !m.Folds("É") {
		t.Fatal("escaped non-ASCII name", m.Name())
	}
}

// A body of many tiny members must not cost memory per member: callers
// iterate, compare names and rewrite without collecting anything.
func TestIterationDoesNotAllocatePerMember(t *testing.T) {
	body := []byte("{" + strings.Repeat(`"key":1,`, 10000) + `"model":"a"}`)
	object, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(10, func() {
		for m := range object.Members() {
			if m.Folds("MODEL") && !m.Is("model") {
				t.Fatal("misread name")
			}
		}
		if _, _, err := object.Unique("model"); err != nil {
			t.Fatal(err)
		}
	})
	if allocations > 4 {
		t.Fatalf("%v allocations for 10001 members", allocations)
	}
}

func TestUnique(t *testing.T) {
	object, _ := Parse([]byte(`{"model":"a","stream":true,"Stream":false,"n":1,"n":2}`))
	if m, ok, err := object.Unique("model"); err != nil || !ok || string(m.Value()) != `"a"` {
		t.Fatal("unique member", m, ok, err)
	}
	if _, ok, err := object.Unique("missing"); err != nil || ok {
		t.Fatal("missing member", ok, err)
	}
	for _, name := range []string{"stream", "n"} {
		if _, _, err := object.Unique(name); err != ErrAmbiguous {
			t.Fatal("ambiguous member accepted", name)
		}
	}
}

func TestWithKeepsOtherBytesAndOwnsFields(t *testing.T) {
	body := ` {"model" : "a", "messages":[{"content":"<b>&amp;</b> é"}], "Priority":-100, "priority":-50, "big":123456789012345678901234567890 } `
	object, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out := object.With(Field{"model", []byte(`"upstream"`)}, Field{"priority", []byte(`3`)}, Field{"store", []byte(`false`)})
	want := ` {"model":"upstream","messages":[{"content":"<b>&amp;</b> é"}],"priority":3,"big":123456789012345678901234567890,"store":false} `
	if string(out) != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
	empty, _ := Parse([]byte(`{}`))
	if got := string(empty.With(Field{"stream_options", []byte(`{"include_usage":true}`)})); got != `{"stream_options":{"include_usage":true}}` {
		t.Fatal(got)
	}
	nested, _ := Parse([]byte(`{"stream_options":{"include_usage":false,"future":1}}`))
	options, _ := nth(nested, 0).Object()
	updated := options.With(Field{"include_usage", []byte("true")})
	if got := string(nested.With(Field{"stream_options", updated})); got != `{"stream_options":{"include_usage":true,"future":1}}` {
		t.Fatal(got)
	}
	if string(object.data) != body {
		t.Fatal("With modified its input")
	}
}

// Whatever members a body contains, every decoder must read the router's
// value for each field written: last-wins and first-wins decoders, and those
// that match names without regard to case.
func FuzzWithOwnsFields(f *testing.F) {
	for _, seed := range seeds {
		f.Add([]byte(seed), "priority", "model")
	}
	f.Add([]byte(`{"PRIORITY":1,"priority":2,"Priority":3}`), "priority", "store")
	f.Fuzz(func(t *testing.T, data []byte, first, second string) {
		object, err := Parse(data)
		if err != nil || strings.EqualFold(first, second) || !utf8.ValidString(first) || !utf8.ValidString(second) {
			return
		}
		out := object.With(Field{first, []byte(`"router-one"`)}, Field{second, []byte(`2`)})
		rewritten, err := Parse(out)
		if err != nil {
			t.Fatalf("invalid output %q: %v", out, err)
		}
		var kept []pair
		for m := range object.Members() {
			if !strings.EqualFold(m.Name(), first) && !strings.EqualFold(m.Name(), second) {
				kept = append(kept, pair{m.Name(), string(m.Value())})
			}
		}
		var others []pair
		seen := map[string]int{}
		for m := range rewritten.Members() {
			switch {
			case m.Folds(first) != strings.EqualFold(m.Name(), first), m.Is(first) != (m.Name() == first):
				t.Fatalf("name comparison disagrees with the decoded name %q", m.Name())
			case strings.EqualFold(m.Name(), first):
				seen[first]++
				if m.Name() != first || string(m.Value()) != `"router-one"` {
					t.Fatalf("field %q survived as %q=%s", first, m.Name(), m.Value())
				}
			case strings.EqualFold(m.Name(), second):
				seen[second]++
				if m.Name() != second || string(m.Value()) != `2` {
					t.Fatalf("field %q survived as %q=%s", second, m.Name(), m.Value())
				}
			default:
				others = append(others, pair{m.Name(), string(m.Value())})
			}
		}
		if seen[first] != 1 || seen[second] != 1 {
			t.Fatalf("fields written %v times", seen)
		}
		if !slices.Equal(others, kept) {
			t.Fatalf("other members changed: %q, want %q", others, kept)
		}
	})
}

func BenchmarkParse(b *testing.B) {
	body := benchmarkBody()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWith(b *testing.B) {
	body := benchmarkBody()
	object, _ := Parse(body)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_ = object.With(Field{"model", []byte(`"upstream"`)})
	}
}

// benchmarkBody is a 512 KiB chat request, mostly message text.
func benchmarkBody() []byte {
	var messages []map[string]string
	for range 512 {
		messages = append(messages, map[string]string{"role": "user", "content": strings.Repeat("a <tagged> & \"quoted\" word ", 37)})
	}
	body, _ := json.Marshal(map[string]any{"model": "chat-model", "stream": true, "max_tokens": 512, "messages": messages})
	return body
}
