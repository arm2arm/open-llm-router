package apierr

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestOpenAIErrorDecodesIntoTypedClient(t *testing.T) {
	w := httptest.NewRecorder()
	WriteDetails(w, config.ProtocolOpenAI, 429, "rate_limit_error", "too many requests", map[string]any{"limit": "tpm"})
	var body struct {
		Error struct {
			Code  *string `json:"code"`
			Type  string  `json:"type"`
			Limit string  `json:"limit"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 429 || body.Error.Code == nil || *body.Error.Code != "rate_limit_error" || body.Error.Type != "rate_limit_error" || body.Error.Limit != "tpm" {
		t.Fatal(w.Body.String())
	}
}

func TestAnthropicErrorEnvelopeUnchanged(t *testing.T) {
	w := httptest.NewRecorder()
	Write(w, config.ProtocolAnthropic, 503, "overloaded_error", "unavailable")
	if w.Code != 503 || w.Body.String() != "{\"error\":{\"message\":\"unavailable\",\"type\":\"overloaded_error\"},\"type\":\"error\"}\n" {
		t.Fatal(w.Body.String())
	}
}
