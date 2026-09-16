package providers

import (
	"context"
	"strings"
)

// Image generation has two possible backends. The rest of the codebase calls
// ImageGenerate and never names a provider unless a node asks for one, so this
// file is the only place the choice is made — the same arrangement text has in
// textrouter.go.

// ImageProviders is the set of providers that can back an image node.
var ImageProviders = []string{"google", "bfl"}

// resolveImageProvider picks the backend for one call: the node's own choice
// first, then the deployment default, then whichever credential exists.
//
// Falling back to the credential rather than to a fixed name is what makes the
// free allowance work without a second code path: a run configured with only
// the platform's Black Forest Labs key routes there by itself, and a user who
// has brought their own Google key keeps going to Google.
func (c *Config) resolveImageProvider(requested string) string {
	for _, p := range []string{requested, c.ImageProvider} {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "google", "gemini":
			return "google"
		case "bfl", "blackforestlabs", "flux", "klein":
			return "bfl"
		}
	}
	if strings.TrimSpace(c.GoogleAPIKey) == "" && strings.TrimSpace(c.BFLAPIKey) != "" {
		return "bfl"
	}
	return "google"
}

// ImageGenerate renders a prompt with whichever backend the request or the
// deployment selects.
//
// The model travels with the provider rather than being translated: a Gemini
// model name means nothing to Black Forest Labs and the reverse, so a node that
// names one without the other gets the backend's own default instead of a
// rejected request.
func (c *Config) ImageGenerate(ctx context.Context, provider, model, prompt, aspectRatio, imageSize string, references []ImageResult) (*ImageResult, error) {
	switch c.resolveImageProvider(provider) {
	case "bfl":
		if !bflModels[strings.ToLower(strings.TrimSpace(model))] {
			model = BFLDefaultModel
		}
		return c.bflImageGenerate(ctx, model, prompt, aspectRatio, imageSize, references)
	default:
		return c.GeminiImageGenerate(ctx, model, prompt, aspectRatio, imageSize, references)
	}
}

// ImageCredential returns the credential a backend would use, so a caller can
// check it is present before starting work. It mirrors TextCredential.
func (c *Config) ImageCredential(provider string) string {
	if c.resolveImageProvider(provider) == "bfl" {
		return strings.TrimSpace(c.BFLAPIKey)
	}
	return strings.TrimSpace(c.GoogleAPIKey)
}
