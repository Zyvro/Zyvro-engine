package providers

import (
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
