package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Anthropic adapter translates between this package's OpenAI-shaped
// conversation and the Messages API. These tests pin the wire format in both
// directions against a local server, so the translation is verified without
// spending anyone's credits.

// captureServer records the request body and replies with a canned response.
func captureServer(t *testing.T, reply string) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var gotReq http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotReq = *r
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotReq, &gotBody
}

const plainReply = `{"content":[{"type":"text","text":"Paris."}],"stop_reason":"end_turn","model":"claude-opus-5"}`

func TestAnthropicSendsTheRightCredentialHeader(t *testing.T) {
	cases := []struct {
		name       string
		credential string
		wantHeader string
		wantBeta   bool
	}{
		{"a console API key goes in x-api-key", "sk-ant-api03-abc123", "x-api-key", false},
		{"an OAuth token goes in Authorization with the beta header", "sk-ant-oat01-xyz789", "Authorization", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, req, _ := captureServer(t, plainReply)
			cfg := &Config{AnthropicAPIKey: tc.credential, AnthropicModel: "claude-opus-5", AnthropicBaseURL: srv.URL}

			if _, err := cfg.anthropicComplete(context.Background(), LLMRequest{
				MaxTokens: 64,
				Messages:  []Message{{Role: "user", Content: "hi"}},
			}); err != nil {
				t.Fatalf("call failed: %v", err)
			}

			if req.Header.Get(tc.wantHeader) == "" {
				t.Fatalf("expected the credential on %s, headers were %v", tc.wantHeader, req.Header)
			}
			// Sending an OAuth token as x-api-key is rejected by the API, so the
			// two must never both be set.
			if tc.wantHeader == "Authorization" && req.Header.Get("x-api-key") != "" {
				t.Fatal("an OAuth token must not also be sent as x-api-key")
			}
			if got := req.Header.Get("anthropic-beta") == anthropicOAuth; got != tc.wantBeta {
				t.Fatalf("oauth beta header present = %v, want %v", got, tc.wantBeta)
			}
			if req.Header.Get("anthropic-version") != anthropicVersion {
				t.Fatal("the version header is required on every request")
			}
		})
	}
}

// A system prompt is a separate field on the Messages API, not a message.
func TestAnthropicLiftsTheSystemPrompt(t *testing.T) {
	srv, _, body := captureServer(t, plainReply)
	cfg := &Config{AnthropicAPIKey: "sk-ant-api03-x", AnthropicModel: "claude-opus-5", AnthropicBaseURL: srv.URL}

	_, err := cfg.anthropicComplete(context.Background(), LLMRequest{
		MaxTokens: 64,
		Messages: []Message{
			{Role: "system", Content: "Be terse."},
			{Role: "user", Content: "What is the capital of France?"},
		},
	})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}

	var sent anthropicRequest
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("unreadable request: %v", err)
	}
	if sent.System != "Be terse." {
		t.Fatalf("system = %q, want it lifted out of the message list", sent.System)
	}
	for _, m := range sent.Messages {
		if m.Role == "system" {
			t.Fatal("a system role must not appear in messages")
		}
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", sent.Messages)
	}
}

// The Brain's loop depends on this: an assistant turn with tool calls and the
// tool replies that follow must survive the round trip.
func TestAnthropicTranslatesToolCallsAndResults(t *testing.T) {
	srv, _, body := captureServer(t, plainReply)
	cfg := &Config{AnthropicAPIKey: "sk-ant-api03-x", AnthropicModel: "claude-opus-5", AnthropicBaseURL: srv.URL}

	_, err := cfg.anthropicComplete(context.Background(), LLMRequest{
		MaxTokens: 64,
		Tools: []Tool{{Type: "function", Function: ToolSchema{
			Name:        "get_weather",
			Description: "Look up weather.",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}}},
		Messages: []Message{
			{Role: "user", Content: "weather in Lyon?"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Type: "function",
				Function: ToolFunction{Name: "get_weather", Arguments: `{"city":"Lyon"}`}}}},
			{Role: "tool", ToolCallID: "toolu_1", Content: "18 and sunny"},
			{Role: "tool", ToolCallID: "toolu_2", Content: "error: unknown tool"},
		},
	})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}

	var sent anthropicRequest
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("unreadable request: %v", err)
	}

	if len(sent.Tools) != 1 || sent.Tools[0].Name != "get_weather" || sent.Tools[0].InputSchema == nil {
		t.Fatalf("tools were not translated to input_schema form: %+v", sent.Tools)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("expected user, assistant, and one user message carrying both results; got %d", len(sent.Messages))
	}

	assistant := sent.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 || assistant.Content[0].Type != "tool_use" {
		t.Fatalf("assistant turn = %+v", assistant)
	}
	if assistant.Content[0].ID != "toolu_1" || assistant.Content[0].Name != "get_weather" {
		t.Fatalf("tool_use block lost its identity: %+v", assistant.Content[0])
	}

	// Consecutive tool replies belong in one user message, and an error result
	// must be flagged so the model can react to it.
	results := sent.Messages[2]
	if results.Role != "user" || len(results.Content) != 2 {
		t.Fatalf("tool results = %+v", results)
	}
	if results.Content[0].Type != "tool_result" || results.Content[0].ToolUseID != "toolu_1" {
		t.Fatalf("first result = %+v", results.Content[0])
	}
	if !results.Content[1].IsError {
		t.Fatal("a result whose text starts with error: should be marked is_error")
	}
}

// The reply is a list of blocks; text and tool calls come back mixed.
func TestAnthropicParsesMixedContentBlocks(t *testing.T) {
	reply := `{"content":[
      {"type":"thinking","thinking":""},
      {"type":"text","text":"Let me check."},
      {"type":"tool_use","id":"toolu_9","name":"get_weather","input":{"city":"Lyon"}}
    ],"stop_reason":"tool_use"}`
	srv, _, _ := captureServer(t, reply)
	cfg := &Config{AnthropicAPIKey: "sk-ant-api03-x", AnthropicModel: "claude-opus-5", AnthropicBaseURL: srv.URL}

	resp, err := cfg.anthropicComplete(context.Background(), LLMRequest{
		MaxTokens: 64, Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if resp.Content != "Let me check." {
		t.Fatalf("text = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(resp.ToolCalls))
	}
	call := resp.ToolCalls[0]
	if call.ID != "toolu_9" || call.Function.Name != "get_weather" {
		t.Fatalf("tool call = %+v", call)
	}
	// The Brain parses these arguments as JSON, so they must stay valid JSON.
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %q", call.Function.Arguments)
	}
	if args["city"] != "Lyon" {
		t.Fatalf("arguments = %v", args)
	}
}

// A safety decline returns HTTP 200 with no usable content. Treating that as an
// empty answer would look like the model said nothing.
func TestAnthropicSurfacesARefusal(t *testing.T) {
	srv, _, _ := captureServer(t, `{"content":[],"stop_reason":"refusal"}`)
	cfg := &Config{AnthropicAPIKey: "sk-ant-api03-x", AnthropicModel: "claude-opus-5", AnthropicBaseURL: srv.URL}

	_, err := cfg.anthropicComplete(context.Background(), LLMRequest{
		MaxTokens: 64, Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("a refusal must surface as an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "declined") {
		t.Fatalf("unhelpful refusal error: %v", err)
	}
}

func TestAnthropicRefusesWithoutACredential(t *testing.T) {
	cfg := &Config{AnthropicModel: "claude-opus-5"}
	_, err := cfg.anthropicComplete(context.Background(), LLMRequest{
		MaxTokens: 64, Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected a missing-credential error")
	}
}

// The router is what the rest of the codebase calls.
func TestTextProviderRouting(t *testing.T) {
	cfg := &Config{TextProvider: "anthropic", OllamaModel: "o", AnthropicModel: "a", OpenAIModel: "g"}
	cases := []struct{ requested, want string }{
		{"", "anthropic"},         // deployment default
		{"openai", "openai"},      // node choice wins
		{"ollama", "ollama"},      // node choice wins
		{"nonsense", "anthropic"}, // unknown falls back to the default
	}
	for _, tc := range cases {
		if got := cfg.resolveTextProvider(tc.requested); got != tc.want {
			t.Fatalf("resolveTextProvider(%q) = %q, want %q", tc.requested, got, tc.want)
		}
	}
	if cfg.TextModel("openai") != "g" {
		t.Fatalf("TextModel did not follow the requested provider")
	}
}
