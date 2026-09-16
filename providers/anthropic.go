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

// Anthropic's Messages API. The rest of this package speaks the OpenAI chat
// shape (a flat message list with tool_calls and role "tool" replies), so this
// file is the translation layer in both directions.
//
// Two credential shapes are accepted, told apart by their prefix:
//   - an API key from console.anthropic.com goes in x-api-key
//   - an OAuth token goes in Authorization: Bearer, with the oauth beta header
//
// Sending an OAuth token as x-api-key fails, which is why this is detected
// rather than left to the caller.

const (
	anthropicVersion = "2023-06-01"
	anthropicOAuth   = "oauth-2025-04-20"
)

func (c *Config) anthropicEndpoint() string {
	base := strings.TrimSuffix(strings.TrimSpace(c.AnthropicBaseURL), "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return base + "/v1/messages"
}

// anthropicIsOAuth reports whether a credential is an OAuth token rather than
// an API key. Console keys are prefixed sk-ant-; anything else is treated as a
// bearer token.
func anthropicIsOAuth(cred string) bool {
	return !strings.HasPrefix(strings.TrimSpace(cred), "sk-ant-api")
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicResponse struct {
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Model      string           `json:"model"`
	Usage      struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// toAnthropic converts the package's OpenAI-shaped conversation into the
// Messages API shape: system prompts move to their own field, assistant tool
// calls become tool_use blocks, and tool replies become tool_result blocks on a
// user message.
func toAnthropic(msgs []Message) (system string, out []anthropicMessage) {
	var systems []string
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if s := strings.TrimSpace(m.Content); s != "" {
				systems = append(systems, s)
			}
		case "tool":
			// A tool reply is a user turn carrying the result of one call.
			block := anthropicBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}
			if strings.HasPrefix(strings.ToLower(m.Content), "error:") {
				block.IsError = true
			}
			// Consecutive results belong in one user message.
			if n := len(out); n > 0 && out[n-1].Role == "user" && len(out[n-1].Content) > 0 &&
				out[n-1].Content[0].Type == "tool_result" {
				out[n-1].Content = append(out[n-1].Content, block)
				continue
			}
			out = append(out, anthropicMessage{Role: "user", Content: []anthropicBlock{block}})
		case "assistant":
			blocks := []anthropicBlock{}
			if s := strings.TrimSpace(m.Content); s != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: s})
			}
			for _, c := range m.ToolCalls {
				args := c.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: c.ID, Name: c.Function.Name, Input: json.RawMessage(args),
				})
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
		default:
			if s := strings.TrimSpace(m.Content); s == "" {
				continue
			}
			out = append(out, anthropicMessage{Role: "user", Content: []anthropicBlock{{Type: "text", Text: m.Content}}})
		}
	}
	return strings.Join(systems, "\n\n"), out
}

// anthropicComplete calls the Messages API and converts the reply back into
// the package's common response shape.
func (c *Config) anthropicComplete(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	if strings.TrimSpace(c.AnthropicAPIKey) == "" {
		return nil, &ProviderError{Code: "auth_error", Message: "no Anthropic credential configured"}
	}
	model := req.Model
	if model == "" {
		model = c.AnthropicModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	system, msgs := toAnthropic(req.Messages)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no messages to send")
	}
	payload := anthropicRequest{Model: model, MaxTokens: maxTokens, System: system, Messages: msgs}
	// Temperature is rejected alongside thinking on the current models, so it is
	// only sent when a caller deliberately asked for one.
	if req.Temperature > 0 && req.Temperature != 0.7 {
		t := req.Temperature
		payload.Temperature = &t
	}
	for _, t := range req.Tools {
		payload.Tools = append(payload.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}

	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.anthropicEndpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	cred := strings.TrimSpace(c.AnthropicAPIKey)
	if anthropicIsOAuth(cred) {
		httpReq.Header.Set("Authorization", "Bearer "+cred)
		httpReq.Header.Set("anthropic-beta", anthropicOAuth)
	} else {
		httpReq.Header.Set("x-api-key", cred)
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return nil, normalizeHTTPError(resp.StatusCode, raw)
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("invalid Anthropic response: %s", truncate(string(raw), 200))
	}
	if parsed.Error != nil {
		return nil, &ProviderError{Code: parsed.Error.Type, Message: parsed.Error.Message}
	}
	// A safety decline comes back as a normal 200 with no usable content, so it
	// is surfaced as an error rather than an empty answer.
	if parsed.StopReason == "refusal" {
		return nil, &ProviderError{Code: "refusal", Message: "the model declined this request"}
	}

	out := &LLMResponse{}
	var texts []string
	for _, b := range parsed.Content {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			args := string(b.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID: b.ID, Type: "function",
				Function: ToolFunction{Name: b.Name, Arguments: args},
			})
		}
	}
	out.Content = strings.TrimSpace(strings.Join(texts, "\n"))
	// Anthropic says "max_tokens" where the OpenAI-shaped providers say
	// "length". Same situation, same answer, so it is translated here rather
	// than taught to the shared check.
	reason := parsed.StopReason
	if reason == "max_tokens" {
		reason = "length"
	}
	if err := emptyCompletion(out.Content, len(out.ToolCalls), reason, parsed.Usage.OutputTokens, req.MaxTokens); err != nil {
		return nil, err
	}
	return out, nil
}
