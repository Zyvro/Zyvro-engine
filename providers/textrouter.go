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
var TextProviders = []string{
	"ollama", "anthropic", "openai",
	claudeCLIProvider, codexCLIProvider,
	OllamaLocalProvider, LMStudioProvider, CustomProvider,
}

// LocalOnlyProviders are the ones that only work where the engine runs on the
// user's own machine: the subprocess CLIs, and the servers listening on it.
//
// Said here rather than left for a caller to know, because a hosted service has
// to keep this exact distinction and was keeping its own copy of the list. Two
// lists mean one of them is wrong the day a provider is added — the engine
// would grow a fourth hosted backend and the hosted service would quietly go on
// refusing it.
// The OpenAI-compatible endpoints are local too, and for a sharper reason than
// the CLIs: a hosted service cannot reach a server on somebody's laptop, and
// letting an account name an arbitrary URL for the server to call would be
// handing it a way to probe the inside of our own network. They belong to the
// machine the person is sitting at.
var LocalOnlyProviders = append(
	[]string{claudeCLIProvider, codexCLIProvider},
	AddressConfiguredProviders...,
)

// IsCLIProvider says whether a provider is a command line tool this engine
// drives as a subprocess. Asked here rather than listed by each caller: the
// daemon's catalogue had its own copy of this answer.
func IsCLIProvider(provider string) bool {
	return provider == claudeCLIProvider || provider == codexCLIProvider
}

// HostedTextProviders is TextProviders minus the ones that need a local
// machine: what a server can actually offer.
func HostedTextProviders() []string { return hostedOnly(TextProviders) }

// HostedImageProviders is the same subtraction for image generation.
func HostedImageProviders() []string { return hostedOnly(ImageProviders) }

// HostedCompletionProviders is the same subtraction for code completion.
func HostedCompletionProviders() []string { return hostedOnly(CompletionProviders) }

// HostedVisionProviders is the same subtraction for vision.
//
// It exists because the local endpoints do both jobs, so the hosted service
// needed the exclusion in two places — and the version of this where the second
// place kept its own list is the version where a vision node can name a backend
// the server cannot reach and nothing says why.
func HostedVisionProviders() []string { return hostedOnly(VisionProviders) }

// HostedVideoProviders is the same subtraction for video. Both of its backends
// are hosted, so it subtracts nothing today — and it exists anyway, because the
// day a video model runs on somebody's own machine is the day a copy of this
// list written by hand would send a hosted run there.
func HostedVideoProviders() []string { return hostedOnly(VideoProviders) }

// hostedOnly drops the providers that only exist on the machine the person is
// sitting at.
func hostedOnly(all []string) []string {
	local := map[string]bool{}
	for _, p := range LocalOnlyProviders {
		local[p] = true
	}
	out := make([]string, 0, len(all))
	for _, p := range all {
		if !local[p] {
			out = append(out, p)
		}
	}
	return out
}

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
		case OllamaLocalProvider:
			return OllamaLocalProvider
		case LMStudioProvider:
			return LMStudioProvider
		case CustomProvider:
			return CustomProvider
		}
	}
	return c.PreferredOrDefault("text", func(p string) bool {
		return strings.TrimSpace(c.TextCredentialFor(p)) != ""
	}, func() string { return "ollama" })
}

// TextCredentialFor answers for one named provider without resolving, which is
// what a preference walk needs: resolving would call back into this and loop.
func (c *Config) TextCredentialFor(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic":
		return c.AnthropicAPIKey
	case "openai":
		return c.OpenAIAPIKey
	case "ollama":
		return c.OllamaAPIKey
	case claudeCLIProvider, codexCLIProvider:
		bin, _ := c.localCLIBinary(strings.ToLower(strings.TrimSpace(provider)))
		if path, err := exec.LookPath(bin); err == nil {
			return path
		}
		return ""
	case OllamaLocalProvider, LMStudioProvider, CustomProvider:
		// There is no secret. Having an address is the whole of being
		// configured, and callers only ever test this for emptiness.
		return c.endpointFor(strings.ToLower(strings.TrimSpace(provider))).URL
	}
	return ""
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
	case OllamaLocalProvider, LMStudioProvider, CustomProvider:
		return c.openAICompatibleComplete(ctx, p, req)
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
