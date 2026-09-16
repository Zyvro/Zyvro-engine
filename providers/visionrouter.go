package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Looking at an image has more than one backend, and for a while this codebase
// behaved as though it had one.
//
// Vision was Gemini's alone, so somebody holding an Ollama key — or running on
// the free allowance, which lends exactly that — was told to go and get a
// Google key for a job their own credential could already do. Most of the
// models Ollama serves today carry a `vision` tag, and the OpenAI-compatible
// endpoint it exposes takes images the OpenAI way, so there was never a
// technical reason for the restriction. There was only a missing router.
//
// This is that router, arranged like textrouter.go and imagerouter.go: the rest
// of the codebase calls VisionAsk and never names a provider unless a node asks
// for one.

// VisionProviders is the set of backends that can answer a question about an
// image. Named here and derived elsewhere, so a list of them never has to be
// written down twice.
var VisionProviders = []string{"google", "ollama", "openai"}

// resolveVisionProvider picks the backend for one call: the request's own
// choice first, then the deployment default for text (which is where an
// Ollama or OpenAI credential would have come from), then whichever credential
// actually exists.
//
// Falling back to the credential rather than to a fixed name is what makes the
// free allowance work without a second code path, exactly as it does for
// images: a run carrying only a lent Ollama key routes there by itself.
func (c *Config) resolveVisionProvider(requested string) string {
	for _, p := range []string{requested} {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "google", "gemini":
			return "google"
		case "ollama":
			return "ollama"
		case "openai", "chatgpt":
			return "openai"
		}
	}
	// The account's own order first; failing that, a credential that is present
	// beats one that is not, in the order a deployment is most likely to have
	// configured them.
	return c.PreferredOrDefault("vision", c.hasVisionCredential, func() string {
		if strings.TrimSpace(c.GoogleAPIKey) != "" {
			return "google"
		}
		if strings.TrimSpace(c.OllamaAPIKey) != "" || strings.TrimSpace(c.OllamaURL) != "" {
			return "ollama"
		}
		if strings.TrimSpace(c.OpenAIAPIKey) != "" {
			return "openai"
		}
		return "google"
	})
}

func (c *Config) hasVisionCredential(provider string) bool {
	switch provider {
	case "google":
		return strings.TrimSpace(c.GoogleAPIKey) != ""
	case "ollama":
		return strings.TrimSpace(c.OllamaAPIKey) != "" || strings.TrimSpace(c.OllamaURL) != ""
	case "openai":
		return strings.TrimSpace(c.OpenAIAPIKey) != ""
	}
	return false
}

// ResolvedVisionProvider says which backend a request would land on, so a
// caller can decide what else to send with it — a model name, in particular,
// which belongs to one backend and means nothing to another.
func (c *Config) ResolvedVisionProvider(requested string) string {
	return c.resolveVisionProvider(requested)
}

// VisionCredential returns the credential a backend would use, so a caller can
// check it is present before starting work. It mirrors TextCredential and
// ImageCredential.
//
// Ollama is the exception and deliberately so: a local Ollama needs no key at
// all, and demanding one would refuse the one setup that costs nothing.
func (c *Config) VisionCredential(provider string) string {
	switch c.resolveVisionProvider(provider) {
	case "ollama":
		if key := strings.TrimSpace(c.OllamaAPIKey); key != "" {
			return key
		}
		return strings.TrimSpace(c.OllamaURL)
	case "openai":
		return strings.TrimSpace(c.OpenAIAPIKey)
	default:
		return strings.TrimSpace(c.GoogleAPIKey)
	}
}

// VisionAsk answers a question about one or more images.
//
// The model travels with the provider rather than being translated: a Gemini
// model name means nothing to Ollama and the reverse, so a caller that names
// one without the other gets the backend's own default instead of a rejected
// request.
func (c *Config) VisionAsk(ctx context.Context, provider, model, instruction string, images [][]byte, mimeTypes []string) (string, error) {
	if len(images) == 0 {
		return "", fmt.Errorf("no image to look at")
	}
	switch c.resolveVisionProvider(provider) {
	case "ollama":
		return c.openAIStyleVision(ctx, "ollama", model, instruction, images, mimeTypes)
	case "openai":
		return c.openAIStyleVision(ctx, "openai", model, instruction, images, mimeTypes)
	default:
		return c.GeminiVision(ctx, model, instruction, images, mimeTypes)
	}
}

// visionContent is one part of an OpenAI-style multimodal message. The format
// is the one both Ollama's compatible endpoint and OpenAI's own take: a list of
// parts, each either text or an image given as a data URL.
type visionContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *visionImageURL `json:"image_url,omitempty"`
}

type visionImageURL struct {
	URL string `json:"url"`
}

type visionMessage struct {
	Role    string          `json:"role"`
	Content []visionContent `json:"content"`
}

type visionRequest struct {
	Model    string          `json:"model"`
	Messages []visionMessage `json:"messages"`
}

// openAIStyleVision serves both Ollama and OpenAI, because both speak the same
// shape here. Writing it twice would be writing the same format down twice.
func (c *Config) openAIStyleVision(ctx context.Context, backend, model, instruction string, images [][]byte, mimeTypes []string) (string, error) {
	parts := make([]visionContent, 0, len(images)+1)
	if strings.TrimSpace(instruction) == "" {
		instruction = "Describe this image."
	}
	parts = append(parts, visionContent{Type: "text", Text: instruction})
	for i, img := range images {
		mime := "image/png"
		if i < len(mimeTypes) && strings.TrimSpace(mimeTypes[i]) != "" {
			mime = mimeTypes[i]
		}
		// A data URL rather than an address: the image is bytes we already
		// hold, and handing over a URL would mean the backend had to be able to
		// reach it — which for a local Ollama and a file on this machine is a
		// requirement nobody can satisfy.
		parts = append(parts, visionContent{
			Type:     "image_url",
			ImageURL: &visionImageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)},
		})
	}

	payload := visionRequest{
		Model:    c.visionModelFor(backend, model),
		Messages: []visionMessage{{Role: "user", Content: parts}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url, key := c.visionEndpoint(backend)
	raw, err := postJSON(ctx, url, key, body)
	if err != nil {
		return "", err
	}

	var parsed ollamaChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("invalid vision response: %s", truncate(string(raw), 200))
	}
	if parsed.Error != nil {
		return "", &ProviderError{Code: "provider_error", Message: parsed.Error.Message}
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("the model returned no answer about the image")
	}
	answer := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if answer == "" {
		// The same distinction the text adapters make: a model that stopped at
		// its limit did not decline, it ran out of room, and saying so is the
		// difference between a bug report and a setting to change.
		return "", emptyCompletion(answer, 0, parsed.Choices[0].FinishReason, parsed.Usage.CompletionTokens, 0)
	}
	return answer, nil
}

// visionModelFor picks the model to ask.
//
// A caller that named one gets it. A caller that did not gets the deployment's
// model for *that* backend and never another's: VisionModel is the Gemini
// vision model — its environment variable says so — and sending that name to
// Ollama would be sending a name nobody there has heard of. An empty string is
// a real answer, and the only honest default when the list of vision-capable
// models changes week to week: it lets the backend choose.
func (c *Config) visionModelFor(backend, requested string) string {
	if m := strings.TrimSpace(requested); m != "" {
		return m
	}
	if backend == "openai" {
		return strings.TrimSpace(c.OpenAIModel)
	}
	return strings.TrimSpace(c.OllamaModel)
}

// visionEndpoint is where the request goes and what authenticates it.
func (c *Config) visionEndpoint(backend string) (url, key string) {
	if backend == "openai" {
		// OpenAIBaseURL already carries the version segment, which is why this
		// appends only the path and not another /v1.
		base := strings.TrimSuffix(strings.TrimSpace(c.OpenAIBaseURL), "/")
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return base + "/chat/completions", strings.TrimSpace(c.OpenAIAPIKey)
	}
	base := strings.TrimSuffix(strings.TrimSpace(c.OllamaURL), "/")
	if base == "" {
		base = "https://ollama.com"
	}
	return base + "/v1/chat/completions", strings.TrimSpace(c.OllamaAPIKey)
}

// postJSON sends one request and hands back the body. A bearer token is
// attached only when there is one: a local Ollama needs none, and sending an
// empty Authorization header is how a request that would have worked gets
// refused.
func postJSON(ctx context.Context, url, key string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return nil, normalizeHTTPError(resp.StatusCode, raw)
	}
	return raw, nil
}
