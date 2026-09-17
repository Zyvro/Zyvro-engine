package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Vision used to be Gemini's alone, so somebody holding an Ollama key — or
// running on the free allowance, which lends exactly that — was told to go and
// get a Google key for a job their own credential could already do.
//
// These tests are mostly about which backend a call lands on and which model
// name goes with it, because both fail quietly. A request routed to the wrong
// backend fails with that backend's error about a model it has never heard of,
// and nothing in the message says the routing was the problem.

// visionServer stands in for an OpenAI-compatible endpoint and records what it
// was asked.
func visionServer(t *testing.T, answer string) (*httptest.Server, *visionRequest, *string) {
	t.Helper()
	var seen visionRequest
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + answer + `"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen, &auth
}

func TestAnOllamaKeyIsEnoughToLookAtAnImage(t *testing.T) {
	srv, seen, auth := visionServer(t, "a cat")
	cfg := &Config{OllamaURL: srv.URL, OllamaAPIKey: "k", OllamaModel: "qwen3.5"}

	answer, err := cfg.VisionAsk(context.Background(), "", "", "what is this?", [][]byte{{1, 2, 3}}, []string{"image/png"})
	if err != nil {
		t.Fatalf("an Ollama-only account could not look at an image: %v", err)
	}
	if answer != "a cat" {
		t.Fatalf("answer = %q", answer)
	}
	if *auth != "Bearer k" {
		t.Fatalf("the key was not sent: %q", *auth)
	}
	if seen.Model != "qwen3.5" {
		t.Fatalf("model = %q, want the deployment's Ollama model", seen.Model)
	}
	if len(seen.Messages) != 1 || len(seen.Messages[0].Content) != 2 {
		t.Fatalf("the message was not one text part plus one image: %+v", seen.Messages)
	}
	if seen.Messages[0].Content[0].Text != "what is this?" {
		t.Fatalf("the question was lost: %+v", seen.Messages[0].Content[0])
	}
	// The image travels as bytes in a data URL, not as an address: a local
	// Ollama cannot fetch a file from this machine, and handing it a URL would
	// be asking it to.
	img := seen.Messages[0].Content[1]
	if img.ImageURL == nil || !strings.HasPrefix(img.ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("the image was not sent inline: %+v", img)
	}
}

// The model name is the thing that goes wrong silently.
func TestAGeminiModelNameIsNeverSentToOllama(t *testing.T) {
	srv, seen, _ := visionServer(t, "ok")
	// VisionModel is the *Gemini* vision model — its environment variable says
	// so. A router that reached for it as "the vision model" would send a name
	// Ollama has never heard of, and the failure would blame the model.
	cfg := &Config{OllamaURL: srv.URL, OllamaAPIKey: "k", VisionModel: "gemini-3.6-flash"}

	if _, err := cfg.VisionAsk(context.Background(), "ollama", "", "q", [][]byte{{1}}, nil); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if strings.Contains(seen.Model, "gemini") {
		t.Fatalf("a Gemini model name was sent to Ollama: %q", seen.Model)
	}
}

func TestAnAskedForModelWins(t *testing.T) {
	srv, seen, _ := visionServer(t, "ok")
	cfg := &Config{OllamaURL: srv.URL, OllamaModel: "default-one"}
	if _, err := cfg.VisionAsk(context.Background(), "ollama", "minicpm-v4.6", "q", [][]byte{{1}}, nil); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if seen.Model != "minicpm-v4.6" {
		t.Fatalf("model = %q, want the one the caller named", seen.Model)
	}
}

// A local Ollama needs no key at all, and demanding one would refuse the setup
// that costs nothing.
func TestALocalOllamaNeedsNoKey(t *testing.T) {
	srv, _, auth := visionServer(t, "ok")
	cfg := &Config{OllamaURL: srv.URL}
	if _, err := cfg.VisionAsk(context.Background(), "ollama", "", "q", [][]byte{{1}}, nil); err != nil {
		t.Fatalf("a keyless local Ollama was refused: %v", err)
	}
	if *auth != "" {
		t.Fatalf("an empty key was sent as a header: %q", *auth)
	}
	if got := cfg.VisionCredential("ollama"); got == "" {
		t.Fatal("a local Ollama reported no usable credential, so a caller would refuse before trying")
	}
}

func TestTheBackendIsChosenByTheCredentialThatExists(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *Config
		want string
	}{
		{"google when it has a key", &Config{GoogleAPIKey: "g"}, "google"},
		{"ollama when only that", &Config{OllamaAPIKey: "o"}, "ollama"},
		{"openai when only that", &Config{OpenAIAPIKey: "x"}, "openai"},
		{"google wins when both", &Config{GoogleAPIKey: "g", OllamaAPIKey: "o"}, "google"},
		{"nothing falls back to google", &Config{}, "google"},
	} {
		if got := tc.cfg.resolveVisionProvider(""); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	// An explicit request beats every credential: a node that names a backend
	// means it.
	cfg := &Config{GoogleAPIKey: "g", OllamaAPIKey: "o"}
	if got := cfg.resolveVisionProvider("ollama"); got != "ollama" {
		t.Fatalf("an explicit choice was overridden: %q", got)
	}
}

func TestAnEmptyAnswerIsExplainedRatherThanReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"length"}],"usage":{"completion_tokens":64}}`))
	}))
	defer srv.Close()
	cfg := &Config{OllamaURL: srv.URL, OllamaAPIKey: "k"}
	_, err := cfg.VisionAsk(context.Background(), "ollama", "", "q", [][]byte{{1}}, nil)
	if err == nil {
		t.Fatal("an empty answer came back as success")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("the reason was not explained: %v", err)
	}
}

func TestLookingAtNothingIsRefused(t *testing.T) {
	cfg := &Config{OllamaAPIKey: "k"}
	if _, err := cfg.VisionAsk(context.Background(), "ollama", "", "q", nil, nil); err == nil {
		t.Fatal("a call with no image was accepted")
	}
}

func TestALocalEndpointGetsItsOwnVisionModelAndAddress(t *testing.T) {
	// The bug this rules out is the one the hosted backends already taught us:
	// a model name belongs to one backend and means nothing to another. The
	// deployment's OllamaModel is the name of a model on ollama.com, and the
	// machine in front of the person has never heard of it.
	srv, seen, auth := visionServer(t, "a dog")
	c := &Config{
		OllamaModel: "a-hosted-model",
		OllamaURL:   "https://ollama.com",
		Endpoints: map[string]Endpoint{
			LMStudioProvider: {URL: srv.URL, Model: "qwen/qwen3-vl"},
		},
	}
	answer, err := c.VisionAsk(context.Background(), LMStudioProvider, "", "what is this", [][]byte{{1, 2}}, []string{"image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "a dog" {
		t.Errorf("answer: %q", answer)
	}
	if seen.Model != "qwen/qwen3-vl" {
		t.Errorf("asked %q — the hosted model name leaked to the local server", seen.Model)
	}
	if *auth != "" {
		t.Errorf("sent Authorization: %q", *auth)
	}
	if len(seen.Messages) != 1 || len(seen.Messages[0].Content) != 2 {
		t.Fatalf("parts: %+v", seen.Messages)
	}
	if !strings.HasPrefix(seen.Messages[0].Content[1].ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("the image was not inlined: %q", seen.Messages[0].Content[1].ImageURL.URL)
	}
}

func TestALocalVisionCallWithNoModelIsRefused(t *testing.T) {
	c := &Config{Endpoints: map[string]Endpoint{CustomProvider: {URL: "http://127.0.0.1:9/v1"}}}
	_, err := c.VisionAsk(context.Background(), CustomProvider, "", "what is this", [][]byte{{1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no vision model chosen") {
		t.Fatalf("expected a refusal naming the setting, got %v", err)
	}
}

func TestALocalEndpointCountsAsAVisionCredential(t *testing.T) {
	// Somebody running LM Studio and holding no key at all can still look at an
	// image, and the guard that asks "is there a credential" must agree.
	c := &Config{Endpoints: map[string]Endpoint{LMStudioProvider: {Model: "m"}}}
	if c.VisionCredential(LMStudioProvider) == "" {
		t.Error("a configured local endpoint reads as no credential")
	}
	if !c.hasVisionCredential(LMStudioProvider) {
		t.Error("hasVisionCredential says no")
	}
	if got := c.ResolvedVisionProvider(LMStudioProvider); got != LMStudioProvider {
		t.Errorf("resolved to %q", got)
	}
}

// Every install carries Ollama's hosted address as its default, so counting
// that address as a credential made hosted Ollama look configured everywhere.
// Vision, whose fallback prefers whatever is configured, went there from
// machines holding no key at all and came back "Unauthorized" — while the
// server the person actually configured sat unused on their own machine.
func TestTheDefaultOllamaAddressIsNotACredential(t *testing.T) {
	fresh := &Config{OllamaURL: DefaultOllamaURL}
	if fresh.hasVisionCredential("ollama") {
		t.Error("a fresh install claims to have an Ollama")
	}
	if got := fresh.resolveVisionProvider(""); got != "google" {
		t.Errorf("vision went to %q on an install with nothing configured", got)
	}

	// A key is a credential.
	keyed := &Config{OllamaURL: DefaultOllamaURL, OllamaAPIKey: "k"}
	if !keyed.hasVisionCredential("ollama") {
		t.Error("a key is not enough")
	}
	// An address somebody typed is a credential: their own Ollama needs none.
	own := &Config{OllamaURL: "http://192.168.1.20:11434"}
	if !own.hasVisionCredential("ollama") {
		t.Error("a self-hosted Ollama reads as unconfigured")
	}

	// And the whole point: with only a local endpoint configured, that is
	// where a vision node with nothing named lands.
	local := &Config{
		OllamaURL: DefaultOllamaURL,
		Endpoints: map[string]Endpoint{CustomProvider: {URL: "http://127.0.0.1:11434/v1", Model: "gemma3:4b"}},
	}
	if got := local.resolveVisionProvider(""); got != CustomProvider {
		t.Errorf("vision went to %q rather than to the only backend configured", got)
	}
}
