package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// strictQuotaRequest accepts a single text generation whose input and output
// have a conservative local bound. Batch generation, external media and hosted
// tools can expand work beyond the serialized input, so require ordinary quota
// policies instead. Unknown numeric payloads retain their exact JSON values.
func strictQuotaRequest(body []byte, path string, meta requestMeta) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	value, err := strictJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("strict quota requires a JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("strict quota requires one JSON object")
	}
	for key := range fields {
		switch strings.ToLower(key) {
		case "messages", "input", "system", "tools", "audio", "modalities":
			if key != strings.ToLower(key) {
				return nil, errors.New("strict quota requires lower-case input, tool and modality fields")
			}
		}
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if value, present := fields[key]; present {
			number, ok := value.(json.Number)
			if !ok {
				return nil, errors.New("strict quota requires positive integer output token limits")
			}
			n, err := number.Int64()
			if err != nil || n <= 0 || n >= 1<<53 {
				return nil, errors.New("strict quota requires positive integer output token limits below 2^53")
			}
		}
	}
	for _, key := range []string{"n", "best_of"} {
		if value, present := fields[key]; present {
			number, ok := value.(json.Number)
			if !ok {
				return nil, errors.New("strict quota supports only n=1 and best_of=1")
			}
			n, err := number.Int64()
			if err != nil || n != 1 {
				return nil, errors.New("strict quota supports only n=1 and best_of=1")
			}
		}
	}
	if prompt, present := fields["prompt"]; present {
		if _, ok := prompt.(string); !ok {
			return nil, errors.New("strict quota requires a single text prompt; batched or hosted prompts are unsupported")
		}
	}
	for _, key := range []string{"messages", "input", "prompt", "system"} {
		if strictMedia(fields[key]) {
			return nil, errors.New("strict quota supports text input only; image, audio, video and file parts are unsupported")
		}
	}
	if _, present := fields["audio"]; present {
		return nil, errors.New("strict quota supports text output only")
	}
	if value, present := fields["modalities"]; present {
		modalities, ok := value.([]any)
		if !ok {
			return nil, errors.New("strict quota requires text-only output modalities")
		}
		for _, modality := range modalities {
			if modality != "text" {
				return nil, errors.New("strict quota supports text output only")
			}
		}
	}
	if value, present := fields["tools"]; present && value != nil {
		tools, ok := value.([]any)
		if !ok {
			return nil, errors.New("strict quota requires an array of client-executed tools")
		}
		for _, tool := range tools {
			tool, ok := tool.(map[string]any)
			if !ok {
				return nil, errors.New("strict quota requires tool objects")
			}
			for key, kind := range tool {
				if strings.EqualFold(key, "type") && kind != "function" && kind != "custom" {
					return nil, errors.New("strict quota supports client-executed function/custom tools only")
				}
			}
		}
	}
	if path != "/v1/embeddings" {
		if meta.MaxTokens == 0 {
			return nil, errors.New("strict quota requires a positive max_tokens (max_output_tokens for Responses)")
		}
		key := "max_tokens"
		if path == "/v1/responses" {
			key = "max_output_tokens"
			delete(fields, "max_tokens")
			delete(fields, "max_completion_tokens")
		} else if path == "/v1/chat/completions" {
			if _, present := fields["max_completion_tokens"]; present {
				key = "max_completion_tokens"
				delete(fields, "max_tokens")
			}
		}
		fields[key] = meta.MaxTokens
	}
	return json.Marshal(fields)
}

// Reject duplicate nested fields as well: a backend may choose the first value
// where encoding/json would choose the last, hiding a media part from validation.
func strictJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("strict quota request exceeds 64 levels of JSON nesting")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, errors.New("invalid strict quota request JSON")
	}
	delim, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delim {
	case '{':
		fields := map[string]any{}
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok {
				return nil, errors.New("invalid strict quota object field")
			}
			if _, exists := fields[key]; exists {
				return nil, errors.New("strict quota requires unique JSON object fields")
			}
			value, err := strictJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			fields[key] = value
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
			return nil, errors.New("invalid strict quota object")
		}
		return fields, nil
	case '[':
		values := []any{}
		for decoder.More() {
			value, err := strictJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
			return nil, errors.New("invalid strict quota array")
		}
		return values, nil
	}
	return nil, errors.New("invalid strict quota JSON value")
}

func strictMedia(value any) bool {
	switch value := value.(type) {
	case []any:
		for _, part := range value {
			if strictMedia(part) {
				return true
			}
		}
	case map[string]any:
		for key, part := range value {
			if strings.EqualFold(key, "type") {
				if kind, ok := part.(string); ok {
					switch strings.ToLower(kind) {
					case "image", "image_url", "input_image", "audio", "input_audio", "video", "video_url", "input_video", "file", "input_file", "document":
						return true
					}
				}
			}
			switch strings.ToLower(key) {
			case "image", "audio", "image_url", "input_audio", "audio_url", "video_url", "file_data", "file_id":
				return true
			}
			if strictMedia(part) {
				return true
			}
		}
	}
	return false
}
