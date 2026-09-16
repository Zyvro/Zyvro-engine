package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A model that answers with nothing is not a success. These tests pin what the
// three adapters do about it, because the failure it used to cause showed up
// three nodes downstream, on a node that was working fine.

func TestEmptyCompletionExplainsAnExhaustedBudget(t *testing.T) {
	err := emptyCompletion("", 0, "length", 202, 200)
	if err == nil {
		t.Fatal("an answer with no content and no room left was reported as success")
	}
	for _, want := range []string{"200 tokens", "spent 202", "Max tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
}

func TestEmptyCompletionWithoutAReasonStillSaysSo(t *testing.T) {
	if err := emptyCompletion("", 0, "stop", 0, 0); err == nil {
		t.Fatal("an empty answer was passed on as a result")
	}
}

// An answer is an answer, whatever shape it took.
func TestARealAnswerIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		content   string
		toolCalls int
	}{
		{"prose", "teapot", 0},
		{"whitespace around prose", "  teapot\n", 0},
		// A model that called a tool and wrote nothing did what it was asked.
		{"a tool call and no prose", "", 1},
	} {
		if err := emptyCompletion(tc.content, tc.toolCalls, "length", 10, 10); err != nil {
			t.Errorf("%s was reported as empty: %v", tc.name, err)
		}
	}
}

// The three adapters have to agree, because a workflow is portable between
// them and an empty answer must not depend on who produced it.
func TestEveryAdapterRefusesAnEmptyAnswer(t *testing.T) {
	openAIShaped := `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"completion_tokens":202}}`
	anthropicShaped := `{"content":[],"stop_reason":"max_tokens","usage":{"output_tokens":202}}`

	for _, tc := range []struct {
		name  string
		reply string
		call  func(c *Config) error
	}{
		{"ollama", openAIShaped, func(c *Config) error {
			_, err := c.ollamaComplete(context.Background(), LLMRequest{MaxTokens: 200, Messages: []Message{{Role: "user", Content: "hello"}}})
			return err
		}},
		{"openai", openAIShaped, func(c *Config) error {
			_, err := c.openAIComplete(context.Background(), LLMRequest{MaxTokens: 200, Messages: []Message{{Role: "user", Content: "hello"}}})
			return err
		}},
		{"anthropic", anthropicShaped, func(c *Config) error {
			_, err := c.anthropicComplete(context.Background(), LLMRequest{MaxTokens: 200, Messages: []Message{{Role: "user", Content: "hello"}}})
			return err
		}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, tc.reply)
		}))
		c := &Config{
			OllamaURL: srv.URL, OllamaAPIKey: "k",
			OpenAIBaseURL: srv.URL, OpenAIAPIKey: "k",
			AnthropicBaseURL: srv.URL, AnthropicAPIKey: "k",
		}
		err := tc.call(c)
		if err == nil {
			t.Errorf("%s passed an empty answer on as a result", tc.name)
		} else if !strings.Contains(err.Error(), "before writing anything") {
			t.Errorf("%s did not explain why it was empty: %v", tc.name, err)
		}
		srv.Close()
	}
}
