package usage

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
)

// TimingHeader explicitly opts a client into response usage extensions. It is
// consumed by OLR and never forwarded to a backend.
const TimingHeader = "X-OLR-Usage-Timing"

// Timing adds router-observed request throughput to authoritative usage. It
// captures bounded usage metadata, including for multi-megabyte Responses SSE
// events. Original bytes are emitted immediately, with insertions only before
// a complete usage object's closing brace. No token counts are synthesized.
type Timing struct {
	protocol string
	sse      bool
	began    time.Time
	now      func() time.Time
	emit     func([]byte) error
	json     JSON
	prefix   int
	data     bool
	skip     bool
	lineLen  int
	cr       bool
}

func NewTiming(protocol string, sse bool, began time.Time, emit func([]byte) error) *Timing {
	t := &Timing{protocol: protocol, sse: sse, began: began, now: time.Now, emit: emit}
	t.reset()
	return t
}

func (t *Timing) reset() {
	t.json = JSON{Protocol: t.protocol}
	if t.sse && t.protocol == Responses {
		t.json.UsageDepth = 2
	}
}

func (t *Timing) extension(raw []byte) []byte {
	var totals Totals
	totals.readUsage(raw, t.protocol, false, true)
	if !totals.HasOutput || len(field(raw, "olr_observed_output_tokens_per_second")) != 0 || len(field(raw, "olr_observed_elapsed_seconds")) != 0 {
		return nil
	}
	elapsed := t.now().Sub(t.began).Seconds()
	if elapsed <= 0 {
		return nil
	}
	value, _ := json.Marshal(struct {
		Rate    json.Number `json:"olr_observed_output_tokens_per_second"`
		Elapsed json.Number `json:"olr_observed_elapsed_seconds"`
	}{json.Number(strconv.FormatFloat(float64(totals.OutputTokens)/elapsed, 'g', 6, 64)), json.Number(strconv.FormatFloat(elapsed, 'g', 6, 64))})
	return append([]byte{','}, value[1:len(value)-1]...)
}

func (t *Timing) Write(p []byte) error {
	written := 0
	var err error
	scan := func(start, end int) {
		t.json.scan(p[start:end], func(offset int, raw []byte) {
			if err != nil {
				return
			}
			if extra := t.extension(raw); len(extra) != 0 {
				at := start + offset
				if err = t.emit(p[written:at]); err == nil {
					err = t.emit(extra)
				}
				written = at
			}
		})
	}
	if !t.sse {
		scan(0, len(p))
	} else {
		for i := 0; i < len(p); {
			c := p[i]
			if c == '\r' || c == '\n' {
				if !(t.cr && c == '\n') {
					if t.data {
						t.json.Write([]byte{'\n'})
					}
					if t.lineLen == 0 {
						t.reset()
					}
					t.prefix, t.data, t.skip, t.lineLen = 0, false, false, 0
				}
				t.cr = c == '\r'
				i++
				continue
			}
			t.cr = false
			if !t.data && !t.skip {
				// Match data: incrementally, tolerating the same UTF-8 BOM as
				// the usage observer. Other SSE fields pass through untouched.
				if t.prefix < 0 {
					index := -t.prefix
					if c != "\xef\xbb\xbf"[index] {
						t.skip = true
					} else if index == 2 {
						t.prefix = 0
					} else {
						t.prefix--
					}
				} else if t.prefix == 0 && c == 0xef {
					t.prefix = -1
				} else if c != "data:"[t.prefix] {
					t.skip = true
				} else {
					t.prefix++
					t.data = t.prefix == len("data:")
				}
				t.lineLen++
				i++
				continue
			}
			end := bytes.IndexAny(p[i:], "\r\n")
			if end < 0 {
				end = len(p) - i
			}
			end += i
			if t.data {
				scan(i, end)
			}
			t.lineLen += end - i
			i = end
		}
	}
	if err == nil && written < len(p) {
		err = t.emit(p[written:])
	}
	return err
}
