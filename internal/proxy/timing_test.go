package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

func TestUsageTimingNativeFormatsAndAccounting(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, option := range []string{"", "false", "true"} {
				t.Run(path+"/"+strconv.FormatBool(stream)+"/"+option, func(t *testing.T) {
					counts := `"prompt_tokens":1000,"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":5},"prompt_tokens_details":{"cached_tokens":900}`
					if path == "/v1/responses" {
						counts = `"input_tokens":1000,"output_tokens":7,"output_tokens_details":{"reasoning_tokens":5},"input_tokens_details":{"cached_tokens":900}`
					}
					wire := `{"usage":{` + counts + `,"provider_decode_tokens_per_second":99},"unknown":123456789012345678901234567890}`
					if stream {
						if path == "/v1/responses" {
							wire = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + wire + "}\n\n"
						} else {
							wire = "data: {\"choices\":[],\"usage\":{" + counts + ",\"provider_decode_tokens_per_second\":99}}\n\ndata: [DONE]\n\n"
						}
					}
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == "GET" {
							w.WriteHeader(200)
							return
						}
						if r.Header.Get(usage.TimingHeader) != "" {
							t.Error("OLR timing header leaked to backend")
						}
						kind := "application/json"
						if stream {
							kind = "text/event-stream"
						}
						w.Header().Set("Content-Type", kind)
						w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
						w.Header().Set("ETag", `"provider-body"`)
						_, _ = io.WriteString(w, wire)
					}))
					defer backend.Close()
					cfg := testConfig(backend.URL)
					cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
					records := make(recordChannel, 1)
					_, router := accountingRouter(t, cfg, records)
					headers := make(http.Header)
					if option != "" {
						headers.Set(usage.TimingHeader, option)
					}
					body := `{"model":"alias","messages":[],"input":"test","max_output_tokens":10,"stream":` + strconv.FormatBool(stream) + `,"stream_options":{"include_usage":true}}`
					resp, got := send(t, router.URL, path, body, headers)
					if resp.StatusCode != 200 {
						t.Fatalf("%d %s", resp.StatusCode, got)
					}
					if option != "true" {
						if string(got) != wire || resp.Header.Get("ETag") == "" {
							t.Fatal("changed native passthrough without opt-in")
						}
					} else {
						jsonBody := got
						if stream {
							_, jsonBody, _ = bytes.Cut(got, []byte("data: "))
							jsonBody, _, _ = bytes.Cut(jsonBody, []byte("\n"))
						}
						var raw map[string]json.RawMessage
						if err := json.Unmarshal(jsonBody, &raw); err != nil {
							t.Fatal(err, string(jsonBody))
						}
						if stream && path == "/v1/responses" {
							if err := json.Unmarshal(raw["response"], &raw); err != nil {
								t.Fatal(err)
							}
						}
						// Decode only the extension, leaving provider detail maps intact.
						var timing struct {
							Rate    float64 `json:"olr_observed_output_tokens_per_second"`
							Elapsed float64 `json:"olr_observed_elapsed_seconds"`
						}
						if err := json.Unmarshal(raw["usage"], &timing); err != nil {
							t.Fatal(err)
						}
						if timing.Rate <= 0 || timing.Elapsed <= 0 || math.Abs(timing.Rate*timing.Elapsed-7) > 1e-4 || !bytes.Contains(got, []byte(counts)) || !bytes.Contains(got, []byte(`"provider_decode_tokens_per_second":99`)) {
							t.Fatalf("invalid throughput or changed provider usage: %s", got)
						}
						if resp.Header.Get("ETag") != "" {
							t.Fatal("retained validator for changed response bytes")
						}
					}
					record := records.next(t)
					if record.Status != "200" || record.InputTokens != 1000 || record.OutputTokens != 7 {
						t.Fatalf("timing changed accounting: %+v", record)
					}
				})
			}
		}
	}
}

func TestUsageTimingDoesNotExposeUnrequestedStreamUsage(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()
	_, router := configured(t, testConfig(backend.URL))
	resp, got := send(t, router.URL, "/v1/chat/completions", `{"model":"alias","stream":true}`, http.Header{usage.TimingHeader: {"true"}})
	if resp.StatusCode != 200 || string(got) != "data: [DONE]\n\n" {
		t.Fatalf("%d %s", resp.StatusCode, got)
	}
}

func TestUsageTimingValidationAndUnsupportedResponses(t *testing.T) {
	var requests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			return
		}
		requests.Add(1)
		if r.Header.Get(usage.TimingHeader) != "" {
			t.Error("leaked timing header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"provider refusal"},"usage":{"completion_tokens":7}}`)
	}))
	defer backend.Close()
	_, router := configured(t, testConfig(backend.URL))
	for _, values := range [][]string{{"yes"}, {"true", "false"}, {"true,true"}} {
		resp, _ := send(t, router.URL, "/v1/chat/completions", `{"model":"alias"}`, http.Header{usage.TimingHeader: values})
		if resp.StatusCode != 400 || requests.Load() != 0 {
			t.Fatal("invalid opt-in dispatched inference")
		}
	}
	resp, got := send(t, router.URL, "/v1/chat/completions", `{"model":"alias"}`, http.Header{usage.TimingHeader: {"true"}})
	if resp.StatusCode != 429 || strings.Contains(string(got), "olr_observed") {
		t.Fatal("modified an upstream error response")
	}
}

func TestUsageTimingInterruptedStream(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(strconv.FormatBool(reported), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
				if reported {
					_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":7}}\n\n")
				}
				// EOF without [DONE] must remain an aborted HTTP stream, even
				// when the provider already supplied an authoritative snapshot.
			}))
			defer backend.Close()
			cfg := testConfig(backend.URL)
			cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
			records := make(recordChannel, 1)
			_, router := accountingRouter(t, cfg, records)
			req, _ := http.NewRequest("POST", router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true,"stream_options":{"include_usage":true}}`))
			req.Header.Set(usage.TimingHeader, "true")
			resp, err := router.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil || bytes.Contains(out, []byte("[DONE]")) || bytes.Contains(out, []byte("olr_observed")) != reported {
				t.Fatalf("interrupted stream: %s %v", out, err)
			}
			record := records.next(t)
			if record.Status != "aborted" || (reported && record.OutputTokens != 7) || (!reported && record.OutputTokens != 1) {
				t.Fatalf("changed aborted accounting: %+v", record)
			}
		})
	}
}
