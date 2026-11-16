package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/reqid"
)

// ConnectionTest selects an active route, never an arbitrary probe destination.
// URL is used only for legacy replicas without a reusable backend ID.
type ConnectionTest struct {
	Model   string `json:"model"`
	Backend string `json:"backend"`
	URL     string `json:"url,omitempty"`
	API     string `json:"api"`
}

type ConnectionResult struct {
	OK           bool   `json:"ok"`
	Message      string `json:"message"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	LatencyMS    int64  `json:"latency_ms"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	RequestID    string `json:"request_id,omitempty"`
}

type connectionTarget struct{}

// TestConnection exercises the same forwarding/admission/accounting path as
// inference, authenticated by the portal's fresh administrator session. A
// private context capability pins every attempt to this backend and origin;
// client headers cannot select a replica or bypass its permissions.
func (h *Handler) TestConnection(ctx context.Context, principal auth.Principal, test ConnectionTest) (result ConnectionResult) {
	began := time.Now()
	defer func() { result.LatencyMS = time.Since(began).Milliseconds() }()
	if principal.UserID == "" {
		result.Message = "Sign in before testing a model."
		return
	}
	cfg, _ := h.Snapshot()
	model, exists := cfg.Models[test.Model]
	if !exists {
		result.Message = "Select an active model for this backend."
		return
	}
	var target *config.Replica
	for _, replica := range model.Pool {
		if (test.Backend != "" && replica.Backend == test.Backend) || (test.Backend == "" && replica.Backend == "" && replica.URL == test.URL) {
			target = &replica
			break
		}
	}
	if target == nil {
		result.Message = "This model is not routed to the selected backend. Reload the backend settings."
		return
	}
	protocol, path := config.ProtocolOpenAI, "/v1/chat/completions"
	body := map[string]any{"model": test.Model, "stream": false, "max_tokens": 16,
		"messages": []any{map[string]string{"role": "user", "content": "Reply with only OK."}}}
	switch test.API {
	case "chat":
	case "responses":
		path = "/v1/responses"
		body = map[string]any{"model": test.Model, "stream": false, "store": false, "max_output_tokens": 16, "input": "Reply with only OK."}
	case "messages":
		protocol, path = config.ProtocolAnthropic, "/v1/messages"
	default:
		result.Message = "Choose Chat Completions, Responses or Anthropic Messages."
		return
	}
	if !slices.Contains(model.Protocols, protocol) {
		result.Message = "This model does not enable the selected API protocol."
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, connectionTarget{}, *target)
	ctx = reqid.NewContext(ctx, reqid.New())
	principal.KeyClass = "interactive"
	ctx = auth.NewContext(ctx, principal)
	payload, _ := json.Marshal(body)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	if protocol == config.ProtocolAnthropic {
		request.Header.Set("Anthropic-Version", "2023-06-01")
	}
	w := &connectionWriter{header: http.Header{}}
	aborted := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				if p != http.ErrAbortHandler {
					panic(p)
				}
				aborted = true
			}
		}()
		h.forward(w, request, protocol)
	}()
	result.HTTPStatus, result.RequestID = w.status, reqid.FromContext(ctx)
	if details := reqid.DetailsFromContext(ctx); details != nil {
		result.InputTokens, result.OutputTokens = details.Record.InputTokens, details.Record.OutputTokens
	}
	switch {
	case ctx.Err() != nil:
		result.Message = "Model test timed out or was canceled. Check pod connectivity, backend load and routing."
	case w.overflow:
		result.Message = "The backend returned too much data for a small model test."
	case aborted:
		result.Message = "The backend response was interrupted."
	case w.status < 200 || w.status >= 300:
		// Never echo untrusted upstream error bodies: they may contain URLs,
		// credentials or unrelated content. Status plus request ID aids diagnosis.
		switch w.status {
		case 401, 403:
			result.Message = "Access was refused. Check model permissions and the backend API key."
		case 404, 405, 501:
			result.Message = "The backend does not serve this model or API. Check the selected model ID and API type."
		case 429:
			result.Message = "A request, token or concurrency limit was reached."
		case 503:
			result.Message = "No healthy replica with available capacity, or the backend returned 503. Check backend health and limits."
		case 502, 504:
			result.Message = "OLR could not reach the backend in time. Check pod connectivity, routing and the backend firewall."
		default:
			result.Message = "The model request failed. Check the API, model configuration and server logs using the request ID."
		}
	default:
		var answer struct {
			Object  string `json:"object"`
			Type    string `json:"type"`
			Status  string `json:"status"`
			Choices []struct {
				Message struct{ Role string } `json:"message"`
			} `json:"choices"`
			Output  []json.RawMessage `json:"output"`
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(w.body.Bytes(), &answer) == nil {
			switch test.API {
			case "chat":
				result.OK = len(answer.Choices) > 0 && answer.Choices[0].Message.Role == "assistant"
			case "responses":
				result.OK = answer.Object == "response" && (answer.Status == "completed" || answer.Status == "incomplete") && len(answer.Output) > 0
			case "messages":
				result.OK = answer.Type == "message" && len(answer.Content) > 0
			}
		}
		result.Message = "The backend returned HTTP success without a valid model response for this API."
		if result.OK {
			result.Message = "Model answered through OLR."
		}
	}
	return
}

// A diagnostic never retains arbitrary amounts of provider output or sends it
// to the browser. The normal proxy still observes usage and cancels on overflow.
type connectionWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *connectionWriter) Header() http.Header { return w.header }
func (w *connectionWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *connectionWriter) Flush() { w.WriteHeader(http.StatusOK) }
func (w *connectionWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.body.Len()+len(p) > 64<<10 {
		w.overflow = true
		return 0, io.ErrShortBuffer
	}
	return w.body.Write(p)
}

var _ http.Flusher = (*connectionWriter)(nil)
