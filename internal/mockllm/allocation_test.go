package mockllm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestControlledAllocationBounds(t *testing.T) {
	server := New(Options{})
	for _, dimensions := range []int{0, 1536, maxEmbeddingDimensions, maxEmbeddingDimensions + 1, -1} {
		request := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(fmt.Sprintf(`{"model":"chat-model","input":"bounded input","dimensions":%d}`, dimensions)))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if dimensions < 0 || dimensions > maxEmbeddingDimensions {
			if response.Code != 400 {
				t.Fatalf("oversized dimensions %d: %d", dimensions, response.Code)
			}
			continue
		}
		var embeddings struct {
			Data []struct{ Embedding []float32 }
		}
		expected := dimensions
		if expected == 0 {
			expected = 8
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &embeddings) != nil || len(embeddings.Data) != 1 || len(embeddings.Data[0].Embedding) != expected {
			t.Fatalf("normal embedding %d failed", dimensions)
		}
	}
	for _, tokens := range []string{"8193", "9223372036854775807"} {
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"chat-model","messages":[]}`))
		request.Header.Set("X-Mock-Tokens", tokens)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 400 {
			t.Fatal("oversized token header accepted", response.Code)
		}
	}
	// Individually valid vectors must also fit the response's total budget.
	inputs := make([]string, maxEmbeddingElements/maxEmbeddingDimensions+1)
	body, _ := json.Marshal(map[string]any{"model": "chat-model", "input": inputs, "dimensions": maxEmbeddingDimensions})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(string(body))))
	if response.Code != 400 {
		t.Fatal("oversized vector batch accepted", response.Code)
	}
}
