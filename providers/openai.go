package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAI's chat completions endpoint. This package's message and tool types
// already follow that wire shape (Ollama's cloud endpoint is OpenAI
// compatible), so the only differences here are the base URL, the credential
// and the model.

func (c *Config) openAIComplete(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	if strings.TrimSpace(c.OpenAIAPIKey) == "" {
		return nil, missingCredential("OpenAI")
	}
	model := req.Model
	if model == "" {
		model = c.OpenAIModel
	}
	base := strings.TrimSuffix(c.OpenAIBaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}

	payload := ollamaChatRequest{
		Model:       model,
		Messages:    req.Messages,
		Tools:       req.Tools,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.OpenAIAPIKey))

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return nil, normalizeHTTPError(resp.StatusCode, raw)
	}

	var parsed ollamaChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("invalid OpenAI response: %s", truncate(string(raw), 200))
	}
	if parsed.Error != nil {
		return nil, &ProviderError{Code: "provider_error", Message: parsed.Error.Message}
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("empty OpenAI response")
	}
	return &LLMResponse{
		Content:   parsed.Choices[0].Message.Content,
		ToolCalls: parsed.Choices[0].Message.ToolCalls,
	}, nil
}
