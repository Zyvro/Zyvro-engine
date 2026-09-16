package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// Every model the platform uses must come out of FromEnv with a value. An
// empty one silently falls back to a literal buried in the engine, which is
// invisible from the admin panel and impossible to override.
func TestFromEnvFillsEveryModel(t *testing.T) {
	for _, key := range []string{"OLLAMA_MODEL", "OLLAMA_SMALL_MODEL", "GEMINI_IMAGE_MODEL", "GEMINI_VISION_MODEL"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	cfg := FromEnv()
	for name, got := range map[string]string{
		"OllamaModel":      cfg.OllamaModel,
		"OllamaSmallModel": cfg.OllamaSmallModel,
		"ImageModel":       cfg.ImageModel,
		"VisionModel":      cfg.VisionModel,
	} {
		if got == "" {
			t.Errorf("%s has no default", name)
		}
	}
}

func TestFromEnvHonoursOverrides(t *testing.T) {
	t.Setenv("GEMINI_IMAGE_MODEL", "some-image-model")
	t.Setenv("GEMINI_VISION_MODEL", "some-vision-model")
	t.Setenv("OLLAMA_SMALL_MODEL", "some-small-model")

	cfg := FromEnv()
	if cfg.ImageModel != "some-image-model" {
		t.Errorf("ImageModel = %q", cfg.ImageModel)
	}
	if cfg.VisionModel != "some-vision-model" {
		t.Errorf("VisionModel = %q", cfg.VisionModel)
	}
	if cfg.OllamaSmallModel != "some-small-model" {
		t.Errorf("OllamaSmallModel = %q", cfg.OllamaSmallModel)
	}
}

// A run the platform pays for pins its model. A free allowance where a node
// could name the most expensive model on the endpoint is not an allowance.
func TestAPinnedModelOverridesWhateverTheNodeAsked(t *testing.T) {
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body.Model
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	pinned := &Config{OllamaURL: srv.URL, OllamaAPIKey: "k", OllamaModel: "deployment-default", PinnedTextModel: "the-pinned-one"}
	if _, err := pinned.LLMComplete(context.Background(), LLMRequest{
		Model:    "something-expensive",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if sent != "the-pinned-one" {
		t.Fatalf("model = %q, want the pinned one", sent)
	}

	// With nothing pinned the node still chooses, which is what every run on
	// somebody's own credential does.
	free := &Config{OllamaURL: srv.URL, OllamaAPIKey: "k", OllamaModel: "deployment-default"}
	if _, err := free.LLMComplete(context.Background(), LLMRequest{
		Model:    "their-choice",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if sent != "their-choice" {
		t.Fatalf("model = %q, want the node's own", sent)
	}
}
