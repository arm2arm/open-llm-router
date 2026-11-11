// Package loadtest compares streaming clients directly and through a router.
// It uses an independent SSE/JSON decoder rather than the proxy's inspector.
package loadtest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type sample struct {
	ttft, tps float64
	tokens    int64
	aborted   bool
	err       string
}

func newClient(concurrency int, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns: concurrency * 3, MaxIdleConnsPerHost: concurrency, MaxConnsPerHost: concurrency,
			IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: timeout, DisableCompression: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type streamEvent struct {
	Type    string          `json:"type"`
	Error   json.RawMessage `json:"error"`
	Message struct {
		Usage tokenUsage `json:"usage"`
	} `json:"message"`
	Usage   tokenUsage `json:"usage"`
	Delta   delta      `json:"delta"`
	Choices []struct {
		Delta delta `json:"delta"`
	} `json:"choices"`
}

type tokenUsage struct {
	Completion *int64 `json:"completion_tokens"`
	Output     *int64 `json:"output_tokens"`
}

type delta struct {
	Content   string `json:"content"`
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	Reasoning string `json:"reasoning_content"`
}

func (d delta) content() bool {
	return d.Content != "" || d.Text != "" || d.Thinking != "" || d.Reasoning != ""
}

func request(ctx context.Context, client *http.Client, o Options, endpoint, model, key, protocol string, abort bool) sample {
	body := map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "Load test: reply with a short sentence."}}, "stream": true, "max_tokens": o.Tokens}
	path := "/v1/chat/completions"
	if protocol == "anthropic" {
		path = "/v1/messages"
	} else {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	encoded, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(encoded))
	if err != nil {
		return sample{err: "create request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if key != "" {
		if protocol == "anthropic" {
			req.Header.Set("X-Api-Key", key)
		} else {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if o.Mock {
		req.Header.Set("X-Mock-Tokens", fmt.Sprint(o.Tokens))
		if o.MockDelay != "" {
			req.Header.Set("X-Mock-Token-Delay", o.MockDelay)
		}
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// Transport errors may include URLs. Report a bounded category only.
		if ctx.Err() != nil {
			return sample{err: "request cancelled or timed out"}
		}
		return sample{err: "transport error"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return sample{err: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return sample{err: "response is not SSE"}
	}
	result := readStream(resp.Body, protocol, started, abort, o.AbortAfter)
	if o.Mock && !result.aborted && result.err == "" && result.tokens != int64(o.Tokens) {
		result.err = "mock output token count differs from requested count"
	}
	return result
}

// readStream bounds each SSE event to 1 MiB and never retains generated text.
// Timing starts at the first content delta, not the role/message_start event.
func readStream(body io.Reader, protocol string, started time.Time, abort bool, abortAfter int) sample {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data []byte
	var first, last time.Time
	var output *int64
	events := 0
	complete := false
	dispatch := func() (stop bool, err error) {
		if len(data) == 0 {
			return false, nil
		}
		if string(data) == "[DONE]" && protocol == "openai" {
			complete = true
			return true, nil
		}
		var event streamEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return true, errors.New("invalid SSE JSON")
		}
		if event.Type == "error" || len(event.Error) > 0 && string(event.Error) != "null" {
			return true, errors.New("SSE error event")
		}
		content := false
		if protocol == "anthropic" {
			content = event.Type == "content_block_delta" && event.Delta.content()
			if event.Type == "message_delta" && event.Usage.Output != nil {
				output = event.Usage.Output
			}
			if event.Type == "message_stop" {
				complete = true
			}
		} else {
			for _, choice := range event.Choices {
				content = content || choice.Delta.content()
			}
			if event.Usage.Completion != nil {
				output = event.Usage.Completion
			}
		}
		if content {
			last = time.Now()
			if first.IsZero() {
				first = last
			}
			events++
		}
		return complete || abort && events >= abortAfter, nil
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			stop, err := dispatch()
			if err != nil {
				return sample{err: err.Error()}
			}
			data = data[:0]
			if abort && events >= abortAfter && !complete {
				return sample{aborted: true, ttft: float64(first.Sub(started)) / float64(time.Millisecond)}
			}
			if stop {
				break
			}
		} else if bytes.HasPrefix(line, []byte("data:")) {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(line[5:], []byte(" "))...)
			if len(data) > 1<<20 {
				return sample{err: "SSE event exceeds 1 MiB"}
			}
		}
	}
	if scanner.Err() != nil {
		return sample{err: "stream read failed"}
	}
	if !complete || output == nil || *output < 2 || events < 2 || !last.After(first) {
		return sample{err: "incomplete stream, missing usage, or fewer than two content events"}
	}
	if abort {
		return sample{err: "stream completed before intentional disconnect"}
	}
	return sample{ttft: float64(first.Sub(started)) / float64(time.Millisecond), tokens: *output, tps: float64(*output-1) / last.Sub(first).Seconds()}
}
