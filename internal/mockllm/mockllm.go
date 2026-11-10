// Package mockllm is a deterministic stand-in for a vLLM server, so that the
// router can be tested without a GPU. It speaks the OpenAI and the Anthropic
// protocol, streams, and can be told to be slow or to fail.
//
// What it answers is fixed by the request alone:
//
//   - Input tokens are the whitespace-separated words of the prompt (message
//     contents, system prompt, completion prompt, embedding input).
//   - A text answer has Options.Tokens output tokens, or max_tokens if that is
//     smaller; its text is Text of that number.
//   - If the request offers tools, the answer is a call of the first tool (or
//     of the one tool_choice names) with the arguments {}. It counts as
//     ToolCallTokens output tokens. There is no tool call when tool_choice is
//     "none" or when the last message already carries a tool result, so a
//     tool loop ends after one round.
//   - An Anthropic answer starts with a thinking block of ThinkingTokens
//     tokens when the request enables thinking or Options.Thinking is set.
//
// Options are the server's defaults; a request can override them with the
// headers X-Mock-Tokens, X-Mock-Token-Delay, X-Mock-Fail and X-Mock-Thinking.
// GET /_mock/stats reports what the server has received, POST /_mock/reset
// clears that.
//
// The mock keeps the last request, body included. It is a test tool and must
// not be put in front of real users.
package mockllm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ToolCallTokens is the number of output tokens of a tool call: the
	// arguments {} are sent as the two pieces "{" and "}".
	ToolCallTokens = 2
	// ThinkingTokens is the number of output tokens of a thinking block.
	ThinkingTokens = 3
	// ThinkingText is the text of a thinking block.
	ThinkingText = "Considering the request."

	defaultTokens = 16
	defaultModel  = "chat-model"
	maxBody       = 64 << 20
)

// Fail is a way for the mock to misbehave.
type Fail string

const (
	FailNone          Fail = ""
	FailError         Fail = "error"           // answer API requests with HTTP 500
	FailUnhealthy     Fail = "unhealthy"       // answer GET /health with 503; the API still works
	FailDrop          Fail = "drop"            // close the connection before the first byte
	FailDropMidStream Fail = "drop-mid-stream" // close it after half of the answer
)

// Options control how the mock answers.
type Options struct {
	Models     []string      // served model names; any other model gets 404. Default: "chat-model"
	Tokens     int           // output tokens of a text answer. Default: 16
	TokenDelay time.Duration // pause before each output token; 1/TokenDelay is the tokens per second
	Fail       Fail
	Thinking   bool // start every Anthropic answer with a thinking block
}

// set changes one option from its text form. The keys are those of the
// X-Mock-* headers and MOCK_* environment variables.
func (o *Options) set(key, value string) error {
	var err error
	switch key {
	case "tokens":
		o.Tokens, err = strconv.Atoi(value)
		if err == nil && o.Tokens < 0 {
			err = errors.New("must not be negative")
		}
	case "token-delay":
		o.TokenDelay, err = time.ParseDuration(value)
		if err == nil && o.TokenDelay < 0 {
			err = errors.New("must not be negative")
		}
	case "fail":
		o.Fail = Fail(value)
		if !slices.Contains([]Fail{FailNone, FailError, FailUnhealthy, FailDrop, FailDropMidStream}, o.Fail) {
			err = fmt.Errorf("unknown failure mode (known: %s, %s, %s, %s)", FailError, FailUnhealthy, FailDrop, FailDropMidStream)
		}
	case "thinking":
		o.Thinking, err = strconv.ParseBool(value)
	}
	if err != nil {
		return fmt.Errorf("%q: %w", value, err)
	}
	return nil
}

var optionKeys = []string{"tokens", "token-delay", "fail", "thinking"}

// OptionsFromEnv reads MOCK_MODELS (comma-separated), MOCK_TOKENS,
// MOCK_TOKEN_DELAY (such as "20ms"), MOCK_FAIL and MOCK_THINKING. getenv has
// the signature of os.Getenv.
func OptionsFromEnv(getenv func(string) string) (Options, error) {
	var o Options
	for _, model := range strings.Split(getenv("MOCK_MODELS"), ",") {
		if model = strings.TrimSpace(model); model != "" {
			o.Models = append(o.Models, model)
		}
	}
	for _, key := range optionKeys {
		name := "MOCK_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
		if value := getenv(name); value != "" {
			if err := o.set(key, value); err != nil {
				return o, fmt.Errorf("%s=%w", name, err)
			}
		}
	}
	return o, nil
}

// Stats is what the mock has seen since it started or was last reset.
type Stats struct {
	Requests       int      `json:"requests"`        // requests to /v1/...
	StreamsAborted int      `json:"streams_aborted"` // streams whose client went away before the end
	Inflight       int      `json:"inflight"`        // requests to /v1/... being answered right now
	PeakInflight   int      `json:"peak_inflight"`   // the highest value Inflight has had
	Last           *Request `json:"last"`            // the most recent request to /v1/...
}

// Request is a request as the mock received it.
type Request struct {
	Method string          `json:"method"`
	Path   string          `json:"path"` // with the query string
	Header http.Header     `json:"headers"`
	Body   json.RawMessage `json:"body"` // null unless the body was JSON
}

// Server is the mock backend. It is an http.Handler, so a test can start one
// with httptest.NewServer(mockllm.New(mockllm.Options{})).
type Server struct {
	mux *http.ServeMux

	mu     sync.Mutex
	opts   Options
	stats  Stats
	lastID int
}

// New returns a mock that answers as opts say.
func New(opts Options) *Server {
	s := &Server{mux: http.NewServeMux()}
	s.SetOptions(opts)

	s.mux.HandleFunc("GET /health", s.health)
	s.mux.HandleFunc("GET /_mock/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Stats())
	})
	s.mux.HandleFunc("POST /_mock/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	})

	s.mux.HandleFunc("GET /v1/models", s.api(openAI, s.models))
	s.mux.HandleFunc("POST /v1/chat/completions", s.api(openAI, s.chatCompletions))
	s.mux.HandleFunc("POST /v1/completions", s.api(openAI, s.completions))
	s.mux.HandleFunc("POST /v1/embeddings", s.api(openAI, s.embeddings))
	s.mux.HandleFunc("POST /v1/messages", s.api(anthropic, s.messages))
	s.mux.HandleFunc("POST /v1/messages/count_tokens", s.api(anthropic, s.messagesCountTokens))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// SetOptions replaces the server's defaults, for example to make a running
// mock unhealthy in the middle of a test.
func (s *Server) SetOptions(opts Options) {
	if len(opts.Models) == 0 {
		opts.Models = []string{defaultModel}
	}
	if opts.Tokens == 0 {
		opts.Tokens = defaultTokens
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts = opts
}

// Stats returns a snapshot of the counters and the last request.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Reset clears the counters and the last request. Requests that are being
// answered stay counted as in flight.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats = Stats{Inflight: s.stats.Inflight, PeakInflight: s.stats.Inflight}
}

func (s *Server) options() Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts
}

// nextID returns an ID such as "chatcmpl-mock-7". IDs count up from 1, so
// they are the same from run to run.
func (s *Server) nextID(prefix string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastID++
	return fmt.Sprintf("%smock-%d", prefix, s.lastID)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	opts := s.options()
	if err := opts.fromHeader(r.Header); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if opts.Fail == FailUnhealthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

// fromHeader applies the X-Mock-* headers of a request.
func (o *Options) fromHeader(h http.Header) error {
	for _, key := range optionKeys {
		name := "X-Mock-" + key
		if values := h.Values(name); len(values) > 0 {
			if err := o.set(key, values[0]); err != nil {
				return fmt.Errorf("header %s: %w", http.CanonicalHeaderKey(name), err)
			}
		}
	}
	return nil
}

// call is one API request being answered.
type call struct {
	Options // the server's defaults with the request's X-Mock-* headers applied
	proto   protocol
	body    []byte
}

// api wraps the handler of a /v1 route: it reads the body, counts and
// records the request, and applies the options that concern every route.
func (s *Server) api(proto protocol, handle func(http.ResponseWriter, *http.Request, *call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			proto.writeError(w, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
			return
		}
		c := &call{Options: s.begin(r, body), proto: proto, body: body}
		defer s.end()

		if err := c.fromHeader(r.Header); err != nil {
			proto.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		switch c.Fail {
		case FailDrop:
			panic(http.ErrAbortHandler) // net/http closes the connection without a response
		case FailError:
			proto.writeError(w, http.StatusInternalServerError, "mock failure (failure mode \"error\")")
			return
		}
		handle(w, r, c)
	}
}

// begin records an API request and returns the options in force.
func (s *Server) begin(r *http.Request, body []byte) Options {
	last := &Request{Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: json.RawMessage("null")}
	if json.Valid(body) {
		last.Body = body
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Requests++
	s.stats.Inflight++
	s.stats.PeakInflight = max(s.stats.PeakInflight, s.stats.Inflight)
	s.stats.Last = last
	return s.opts
}

func (s *Server) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Inflight--
}

// decode reads the request body into v and checks that the model is served.
// It writes the error response itself and reports whether the call can go on.
func (c *call) decode(w http.ResponseWriter, v any, model *string) bool {
	if err := json.Unmarshal(c.body, v); err != nil {
		c.proto.writeError(w, http.StatusBadRequest, "request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	if !slices.Contains(c.Models, *model) {
		c.proto.writeError(w, http.StatusNotFound, fmt.Sprintf("The model `%s` does not exist.", *model))
		return false
	}
	return true
}

// outputTokens returns the length of a text answer and whether the request's
// own limit cut it short.
func (c *call) outputTokens(limits ...*int) (n int, truncated bool) {
	for _, limit := range limits {
		if limit != nil {
			if *limit < c.Tokens {
				return max(*limit, 0), true
			}
			break
		}
	}
	return c.Tokens, false
}

// wait pauses for the time one output token takes. It reports false when the
// client has gone away in the meantime.
func (c *call) wait(ctx context.Context) bool {
	if c.TokenDelay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(c.TokenDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// respond sends a complete (not streamed) answer of n output tokens after the
// time it takes to generate them.
func (c *call) respond(w http.ResponseWriter, r *http.Request, n int, answer any) {
	for range n {
		if !c.wait(r.Context()) {
			return
		}
	}
	if c.Fail != FailDropMidStream {
		writeJSON(w, http.StatusOK, answer)
		return
	}
	// Announce the whole body, send half of it, and close the connection.
	body, _ := json.Marshal(answer)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body[:len(body)/2])
	http.NewResponseController(w).Flush()
	panic(http.ErrAbortHandler)
}

// stream is a server-sent-events response in progress.
type stream struct {
	s       *Server
	c       *call
	ctx     context.Context
	w       http.ResponseWriter
	total   int // output tokens of the whole answer
	sent    int // output tokens sent so far
	aborted bool
}

// startStream sends the response headers of a stream that will carry total
// output tokens.
func (s *Server) startStream(w http.ResponseWriter, r *http.Request, c *call, total int) *stream {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	http.NewResponseController(w).Flush()
	return &stream{s: s, c: c, ctx: r.Context(), w: w, total: total}
}

// event sends one event at once. With an empty name it is sent the OpenAI
// way, as a data line alone. It reports false when the client has gone away.
func (st *stream) event(name string, data any) bool {
	if st.aborted {
		return false
	}
	var payload []byte
	if text, ok := data.(string); ok {
		payload = []byte(text) // the literal [DONE]
	} else {
		payload, _ = json.Marshal(data)
	}
	var err error
	if name != "" {
		_, err = fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", name, payload)
	} else {
		_, err = fmt.Fprintf(st.w, "data: %s\n\n", payload)
	}
	if err == nil {
		err = http.NewResponseController(st.w).Flush()
	}
	if err != nil || st.ctx.Err() != nil {
		st.abort()
		return false
	}
	return true
}

// token sends an event that carries one output token, after the time that
// token takes.
func (st *stream) token(name string, data any) bool {
	if st.aborted {
		return false
	}
	if st.c.Fail == FailDropMidStream && st.sent == st.total/2 {
		panic(http.ErrAbortHandler)
	}
	if !st.c.wait(st.ctx) {
		st.abort()
		return false
	}
	st.sent++
	return st.event(name, data)
}

func (st *stream) abort() {
	if st.aborted {
		return
	}
	st.aborted = true
	st.s.mu.Lock()
	defer st.s.mu.Unlock()
	st.s.stats.StreamsAborted++
}

// Text returns the text of an answer of n output tokens.
func Text(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(token(i))
	}
	return b.String()
}

var words = []string{"The", "quick", "brown", "fox", "jumps", "over", "the", "lazy", "dog."}

// token returns output token i: a word, with a space in front of all but the
// first, the way tokenizers split text.
func token(i int) string {
	if i == 0 {
		return words[0]
	}
	return " " + words[i%len(words)]
}

// tokens returns the first n output tokens.
func tokens(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = token(i)
	}
	return out
}

// countTokens counts the input tokens of a decoded JSON value: the words of a
// string, one per number (an input given as token IDs), and for lists and
// content blocks the sum over what they hold in "text" and "content".
func countTokens(v any) int {
	switch v := v.(type) {
	case string:
		return len(strings.Fields(v))
	case float64:
		return 1
	case []any:
		n := 0
		for _, item := range v {
			n += countTokens(item)
		}
		return n
	case map[string]any:
		return countTokens(v["text"]) + countTokens(v["content"])
	}
	return 0
}

// message is a chat message of either protocol.
type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func countMessages(messages []message) int {
	n := 0
	for _, m := range messages {
		n += countTokens(m.Content)
	}
	return n
}

// obj builds JSON objects for the responses.
type obj = map[string]any

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// protocol decides the shape of error responses.
type protocol int

const (
	openAI protocol = iota
	anthropic
)

func (p protocol) writeError(w http.ResponseWriter, status int, message string) {
	names := map[int][2]string{
		http.StatusBadRequest:            {"BadRequestError", "invalid_request_error"},
		http.StatusNotFound:              {"NotFoundError", "not_found_error"},
		http.StatusRequestEntityTooLarge: {"BadRequestError", "request_too_large"},
		http.StatusInternalServerError:   {"InternalServerError", "api_error"},
	}[status]
	if p == anthropic {
		writeJSON(w, status, obj{"type": "error", "error": obj{"type": names[1], "message": message}})
		return
	}
	writeJSON(w, status, obj{"error": obj{"message": message, "type": names[0], "param": nil, "code": status}})
}
