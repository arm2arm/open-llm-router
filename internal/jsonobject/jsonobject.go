// Package jsonobject reads and rewrites the members of a JSON object without
// decoding, copying or re-encoding the values the router does not own.
//
// Request bodies are large (long conversations, base64 attachments) while the
// router reads and sets a few small top-level fields. Decoding such a body into
// maps of raw values and encoding it again allocated many times its size.
// Here, validation is a single allocation-free pass (encoding/json's own
// grammar), members are iterated as views into the input without being
// collected, and a rewrite copies the input once with only the replaced values
// changed. No step allocates per member, whatever the shape of the body.
package jsonobject

import (
	"bytes"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"unicode/utf8"
)

var (
	ErrInvalid   = errors.New("invalid JSON")
	ErrNotObject = errors.New("not a JSON object")
	// ErrAmbiguous reports a repeated member name, or a name that matches
	// another only without regard to case: decoders differ in which they use.
	ErrAmbiguous = errors.New("ambiguous JSON member")
)

// Object is a validated JSON object.
type Object struct {
	data  []byte
	open  int // offset of '{'
	close int // offset of '}'
}

// Parse accepts exactly one JSON object, optionally surrounded by whitespace,
// with the grammar of encoding/json (which also accepts invalid UTF-8).
func Parse(data []byte) (Object, error) {
	if !json.Valid(data) {
		return Object{}, ErrInvalid
	}
	open := skipSpace(data, 0)
	if data[open] != '{' {
		return Object{}, ErrNotObject
	}
	// Only whitespace can follow the one valid value.
	close := len(data) - 1
	for data[close] != '}' {
		close--
	}
	return Object{data: data, open: open, close: close}, nil
}

// Member is one name/value pair of an object, or one element of an array.
type Member struct {
	name  []byte // the raw JSON string, quotes included; nil for elements
	plain bool   // name has no escapes and is valid UTF-8: raw equals decoded
	value []byte
	start int // offset of the name's opening quote (of the value, for elements)
	end   int // offset just after the value
}

// Members iterates the object's members in input order.
func (o Object) Members() iter.Seq[Member] {
	return func(yield func(Member) bool) {
		data := o.data
		for i := skipSpace(data, o.open+1); data[i] != '}'; {
			nameEnd := stringEnd(data, i)
			name := data[i:nameEnd:nameEnd]
			valueStart := skipSpace(data, skipSpace(data, nameEnd)+1) // past ':'
			end := valueEnd(data, valueStart)
			inner := name[1 : len(name)-1]
			plain := bytes.IndexByte(inner, '\\') < 0 && utf8.Valid(inner)
			if !yield(Member{name: name, plain: plain, value: data[valueStart:end:end], start: i, end: end}) {
				return
			}
			if i = skipSpace(data, end); data[i] == ',' {
				i = skipSpace(data, i+1)
			}
		}
	}
}

// Name is the member name decoded exactly as encoding/json decodes object
// keys, including escapes and the replacement of invalid UTF-8.
func (m Member) Name() string {
	if m.name == nil {
		return ""
	}
	if m.plain {
		return string(m.name[1 : len(m.name)-1])
	}
	var name string
	_ = json.Unmarshal(m.name, &name) // valid by construction
	return name
}

// Is reports whether the decoded name equals name, without allocating for
// names that need no decoding.
func (m Member) Is(name string) bool {
	if m.plain {
		return string(m.name[1:len(m.name)-1]) == name
	}
	return m.name != nil && m.Name() == name
}

// Folds reports whether the decoded name equals name under Unicode case
// folding (strings.EqualFold), without allocating for plain names.
func (m Member) Folds(name string) bool {
	if m.plain {
		return bytes.EqualFold(m.name[1:len(m.name)-1], []byte(name))
	}
	return m.name != nil && strings.EqualFold(m.Name(), name)
}

// Value is the raw JSON value: a view into the parsed input that must not be
// modified. The zero Member has a nil value.
func (m Member) Value() []byte { return m.value }

// Object returns the value as an object, when it is one.
func (m Member) Object() (Object, bool) {
	if len(m.value) == 0 || m.value[0] != '{' {
		return Object{}, false
	}
	return Object{data: m.value, close: len(m.value) - 1}, true
}

// Elements iterates the elements of the value when it is an array, and
// yields nothing otherwise.
func (m Member) Elements() iter.Seq[Member] {
	return func(yield func(Member) bool) {
		data := m.value
		if len(data) == 0 || data[0] != '[' {
			return
		}
		for i := skipSpace(data, 1); data[i] != ']'; {
			end := valueEnd(data, i)
			if !yield(Member{value: data[i:end:end], start: i, end: end}) {
				return
			}
			if i = skipSpace(data, end); data[i] == ',' {
				i = skipSpace(data, i+1)
			}
		}
	}
}

// Unique returns the member spelled exactly name, if present. It returns
// ErrAmbiguous when name appears more than once, or when another member
// matches it only without regard to case.
func (o Object) Unique(name string) (Member, bool, error) {
	var found Member
	present := false
	for m := range o.Members() {
		if !m.Folds(name) {
			continue
		}
		if present || !m.Is(name) {
			return Member{}, false, ErrAmbiguous
		}
		found, present = m, true
	}
	return found, present, nil
}

// Field is a member to write. Name must be valid UTF-8 and Value exactly one
// valid JSON value.
type Field struct {
	Name  string
	Value []byte
}

// With returns a new copy of the object in which each field replaces every
// member whose name matches it without regard to case, so that any decoder
// reads the field's value. A field takes the position of the first such
// member, or is appended. Every other member keeps its original bytes.
func (o Object) With(fields ...Field) []byte {
	size := len(o.data)
	encoded := make([][]byte, len(fields))
	for i, f := range fields {
		name, _ := json.Marshal(f.Name)
		encoded[i] = append(append(name, ':'), f.Value...)
		size += len(encoded[i]) + 1
	}
	out := make([]byte, 0, size)
	out = append(out, o.data[:o.open+1]...)
	written := make([]bool, len(fields))
	first := true
	emit := func(member []byte) {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, member...)
	}
	for m := range o.Members() {
		match := -1
		for i, f := range fields {
			if m.Folds(f.Name) {
				match = i
				break
			}
		}
		switch {
		case match < 0:
			emit(o.data[m.start:m.end])
		case !written[match]:
			written[match] = true
			emit(encoded[match])
		}
	}
	for i := range fields {
		if !written[i] {
			emit(encoded[i])
		}
	}
	return append(out, o.data[o.close:]...)
}

func skipSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// stringEnd returns the offset after the string that opens at data[i]. A
// quote ends it unless an odd run of backslashes precedes it; every run is
// scanned once, so this stays linear on adversarial input.
func stringEnd(data []byte, i int) int {
	for j := i + 1; ; {
		quote := j + bytes.IndexByte(data[j:], '"')
		escapes := 0
		for k := quote - 1; k > i && data[k] == '\\'; k-- {
			escapes++
		}
		if escapes%2 == 0 {
			return quote + 1
		}
		j = quote + 1
	}
}

// valueEnd returns the offset after the value that starts at data[i].
func valueEnd(data []byte, i int) int {
	switch data[i] {
	case '"':
		return stringEnd(data, i)
	case '{', '[':
		depth := 0
		for j := i; ; j++ {
			switch data[j] {
			case '"':
				j = stringEnd(data, j) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
	default: // number, true, false or null
		j := i
		for j < len(data) {
			switch data[j] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return j
			}
			j++
		}
		return j
	}
}
