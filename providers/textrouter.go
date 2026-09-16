package providers

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Text generation has several possible backends. The rest of the codebase calls
// LLMComplete and never names a provider unless a node asks for one, so this
// file is the only place the choice is made.

// TextProviders is the set of providers that can back a text node or the Brain.
// The two -cli entries are subprocess providers and only work where the engine
// runs on the user's own machine.
var TextProviders = []string{"ollama", "anthropic", "openai", "claude-cli", "codex-cli"}

// resolveTextProvider picks the provider for one call: the node's own choice
// first, then the deployment default, then Ollama.
func (c *Config) resolveTextProvider(requested string) string {
	for _, p := range []string{requested, c.TextProvider} {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "anthropic":
			return "anthropic"
		case "openai":
			return "openai"
		case "ollama":
			return "ollama"
		case claudeCLIProvider:
			return claudeCLIProvider
		case codexCLIProvider:
			return codexCLIProvider
		}
	}
	return "ollama"
}

// LLMComplete sends a chat completion to whichever provider the request or the
// deployment selects, and returns one common response shape.
func (c *Config) LLMComplete(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	// A pinned model wins over the node's choice. It is only ever set on a run
	// the platform pays for; see the field's own comment.
	if m := strings.TrimSpace(c.PinnedTextModel); m != "" {
		req.Model = m
	}
	switch p := c.resolveTextProvider(req.Provider); p {
	case "anthropic":
		return c.anthropicComplete(ctx, req)
	case "openai":
		return c.openAIComplete(ctx, req)
	case claudeCLIProvider, codexCLIProvider:
		return c.localCLIComplete(ctx, req, p)
	default:
		return c.ollamaComplete(ctx, req)
	}
}

// TextCredential returns the credential a provider would use, so callers can
// check it is present before starting work.
func (c *Config) TextCredential(provider string) string {
	switch p := c.resolveTextProvider(provider); p {
	case "anthropic":
		return c.AnthropicAPIKey
	case "openai":
		return c.OpenAIAPIKey
	case claudeCLIProvider, codexCLIProvider:
		// A CLI provider has no secret to hand back: the subscription lives in
		// the CLI's own session. Callers only ever test this for emptiness to
		// decide whether the provider is configured, so the answer is the
		// resolved binary path when the CLI is installed and empty when it is
		// not.
		bin, _ := c.localCLIBinary(p)
		path, err := exec.LookPath(bin)
		if err != nil {
			return ""
		}
		return path
	default:
		return c.OllamaAPIKey
	}
}

// TextModel reports the model a provider would use, for display. For the CLI
// providers this is usually empty, meaning the CLI picks its own default.
func (c *Config) TextModel(provider string) string {
	switch c.resolveTextProvider(provider) {
	case "anthropic":
		return c.AnthropicModel
	case "openai":
		return c.OpenAIModel
	case claudeCLIProvider:
		return c.ClaudeCLIModel
	case codexCLIProvider:
		return c.CodexCLIModel
	default:
		return c.OllamaModel
	}
}

func missingCredential(provider string) error {
	return &ProviderError{
		Code:    "auth_error",
		Message: fmt.Sprintf("no %s credential configured", provider),
	}
}
