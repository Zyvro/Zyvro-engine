package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Servers that speak OpenAI's chat API, running wherever the person put them.
//
// Ollama on this machine, LM Studio, and anything else with the same endpoint:
// llama.cpp's server, vLLM, LocalAI, a colleague's box on the LAN. They are one
// provider three times over, because the only thing that differs is the address
// — and the version of this that gave each of them its own adapter would have
// been three copies of one HTTP call, drifting.
//
// Why they are worth having at all: a model on your own machine costs nothing
// per call, works on a plane, and never sends the image you are asking about to
// anybody. That last one is the reason vision matters here in particular.

// Endpoint is one such server: where it is, what opens it, and which model to
// ask when nobody says.
type Endpoint struct {
	// URL is the base, including the version segment: http://127.0.0.1:11434/v1
	URL string
	// Key is usually empty. A server on your own machine authenticates nobody,
	// and demanding a key would refuse the setup that costs nothing.
	Key string
	// Model is what this endpoint answers with when a node names none. There is
	// no sensible default we could ship: what is installed is the person's
	// business, and guessing a name produces a 404 that blames the model.
	Model string
}

// The three ids. Named constants because they cross into the catalogue, the
// routers and the stored preferences, and a typo in any one of those is a
// provider that silently never resolves.
const (
	OllamaLocalProvider = "ollama-local"
	LMStudioProvider    = "lmstudio"
	CustomProvider      = "custom"
	CustomImageProvider = "custom-image"
)

// OpenAICompatibleProviders are the ones this file serves. They do both text
// and vision, because the endpoint does: the same /v1/chat/completions takes a
// string or a list of parts, and which one it gets is the only difference.
var OpenAICompatibleProviders = []string{OllamaLocalProvider, LMStudioProvider, CustomProvider}

// ImageEndpointProviders speak the other half of the same API: the images
// routes, which are a different shape from chat and therefore a different
// adapter — but the same protocol, the same setting, and the same promise that
// nothing leaves the machine. imageendpoint.go serves them.
var ImageEndpointProviders = []string{CustomImageProvider}

// AddressConfiguredProviders is every provider configured by an address rather
// than by a key, whichever job it does. The catalogue and the settings route
// ask this rather than keeping their own idea of which providers have a URL.
var AddressConfiguredProviders = append(
	append([]string{}, OpenAICompatibleProviders...),
	ImageEndpointProviders...,
)

// DefaultEndpointURL is where each address-configured provider listens when
// nobody has said otherwise.
//
// The known ports are the projects' own documented defaults, checked against
// the running servers rather than recalled. Custom has none: the whole point of
// it is that we do not know where it is.
func DefaultEndpointURL(provider string) string {
	switch provider {
	case OllamaLocalProvider:
		return "http://127.0.0.1:11434/v1"
	case LMStudioProvider:
		return "http://127.0.0.1:1234/v1"
	}
	return ""
}

// endpointFor resolves one provider's endpoint, filling in the default address
// when the person turned it on without giving one.
//
// The entry has to exist first. Filling in the default for a provider nobody
// enabled would make every install claim it had Ollama and LM Studio running,
// so "text is covered, no key needed" — and then fail at the first call with a
// connection refused from a port nothing is listening on. Presence in this map
// is the switch.
func (c *Config) endpointFor(provider string) Endpoint {
	e, on := c.Endpoints[provider]
	if !on {
		return Endpoint{}
	}
	if strings.TrimSpace(e.URL) == "" {
		e.URL = DefaultEndpointURL(provider)
	}
	e.URL = strings.TrimSuffix(strings.TrimSpace(e.URL), "/")
	return e
}

// EndpointFor is endpointFor for callers outside this package — the daemon's
// settings panel, which has to show what this project is pointed at, including
// the default address it filled in for somebody who only ticked the box.
func (c *Config) EndpointFor(provider string) Endpoint { return c.endpointFor(provider) }

// isOpenAICompatible says whether an id belongs to this file.
func isOpenAICompatible(provider string) bool {
	for _, p := range OpenAICompatibleProviders {
		if p == provider {
			return true
		}
	}
	return false
}

// EndpointConfigured reports whether a provider has somewhere to talk to. For
// these, that is the whole of being configured: there is no key to check.
func (c *Config) EndpointConfigured(provider string) bool {
	return strings.TrimSpace(c.endpointFor(provider).URL) != ""
}

// openAICompatibleComplete is the text half. It reuses the request and response
// shapes the Ollama adapter already defines, because they are OpenAI's shapes
// and writing them a second time would be writing the same format twice.
func (c *Config) openAICompatibleComplete(ctx context.Context, provider string, req LLMRequest) (*LLMResponse, error) {
	e := c.endpointFor(provider)
	if e.URL == "" {
		return nil, fmt.Errorf("%s has no address configured", provider)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(e.Model)
	}
	if model == "" {
		// Said plainly rather than sent empty: an empty model reaches the
		// server, is refused, and comes back as an error about the model
		// instead of about the setting nobody filled in.
		return nil, fmt.Errorf("%s has no model chosen — pick one in the provider settings", provider)
	}

	body, err := json.Marshal(ollamaChatRequest{
		Model:       model,
		Messages:    req.Messages,
		Tools:       req.Tools,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	})
	if err != nil {
		return nil, err
	}
	raw, err := postJSON(ctx, e.URL+"/chat/completions", e.Key, body)
	if err != nil {
		return nil, err
	}
	return parseOpenAIStyleCompletion(raw, req.MaxTokens)
}

// ModelList is what an endpoint says it can run, which is the only honest
// source for a model picker: what is installed is the person's business and
// changes whenever they pull something new.
func (c *Config) ModelList(ctx context.Context, provider string) ([]string, error) {
	e := c.endpointFor(provider)
	if e.URL == "" {
		return nil, fmt.Errorf("%s has no address configured", provider)
	}
	raw, err := getJSON(ctx, e.URL+"/models", e.Key)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("that address did not answer with a model list")
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}
