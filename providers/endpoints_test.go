package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ollama on this machine, LM Studio and a hand-entered address are one provider
// three times over: the same OpenAI chat endpoint at three places. These tests
// are about the three ways that sameness can be got wrong — sending a request to
// the wrong address, sending another backend's model name, and sending an empty
// model name — because each of those comes back as an error about something
// other than its cause.

// endpointServer stands in for a local server and records what it was asked.
func endpointServer(t *testing.T) (*httptest.Server, *map[string]any, *string, *string) {
	t.Helper()
	seen := map[string]any{}
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"zebra"},{"id":"alpaca"},{"id":"  "}]}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen, &path, &auth
}

func TestEachLocalProviderKeepsItsOwnAddressAndModel(t *testing.T) {
	// The failure this rules out: two endpoints sharing one setting, so turning
	// on LM Studio quietly re-points Ollama at it.
	ollama, seenOllama, _, _ := endpointServer(t)
	lmstudio, seenLM, _, _ := endpointServer(t)
	c := &Config{Endpoints: map[string]Endpoint{
		OllamaLocalProvider: {URL: ollama.URL, Model: "gemma4:latest"},
		LMStudioProvider:    {URL: lmstudio.URL, Model: "qwen/qwen3-coder-next"},
	}}

	for _, p := range []string{OllamaLocalProvider, LMStudioProvider} {
		if _, err := c.LLMComplete(context.Background(), LLMRequest{
			Provider: p,
			Messages: []Message{{Role: "user", Content: "hi"}},
		}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if got := (*seenOllama)["model"]; got != "gemma4:latest" {
		t.Errorf("ollama-local was asked for %v", got)
	}
	if got := (*seenLM)["model"]; got != "qwen/qwen3-coder-next" {
		t.Errorf("lmstudio was asked for %v", got)
	}
}

func TestANamedModelBeatsTheEndpointDefault(t *testing.T) {
	srv, seen, _, _ := endpointServer(t)
	c := &Config{Endpoints: map[string]Endpoint{CustomProvider: {URL: srv.URL, Model: "fallback"}}}
	if _, err := c.LLMComplete(context.Background(), LLMRequest{
		Provider: CustomProvider,
		Model:    "klein-2.0",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := (*seen)["model"]; got != "klein-2.0" {
		t.Errorf("the node's model was ignored: %v", got)
	}
}

func TestAnEmptyModelIsRefusedRatherThanSent(t *testing.T) {
	// An empty model reaches the server and comes back as the server's
	// complaint about the model, which sends the reader looking at models
	// instead of at the setting nobody filled in.
	srv, _, _, _ := endpointServer(t)
	c := &Config{Endpoints: map[string]Endpoint{LMStudioProvider: {URL: srv.URL}}}
	_, err := c.LLMComplete(context.Background(), LLMRequest{
		Provider: LMStudioProvider,
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no model chosen") {
		t.Fatalf("expected a refusal naming the setting, got %v", err)
	}
}

func TestTurningOneOnIsEnoughToReachIt(t *testing.T) {
	// Somebody who ticks "Ollama local" and nothing else has said everything
	// there is to say: the port is the project's own.
	c := &Config{Endpoints: map[string]Endpoint{
		OllamaLocalProvider: {Model: "gemma4:latest"},
		LMStudioProvider:    {},
	}}
	if got := c.endpointFor(OllamaLocalProvider).URL; got != "http://127.0.0.1:11434/v1" {
		t.Errorf("default address: %q", got)
	}
	if got := c.endpointFor(LMStudioProvider).URL; got != "http://127.0.0.1:1234/v1" {
		t.Errorf("lmstudio default address: %q", got)
	}
	// Custom has no default on purpose: the whole point of it is that we do not
	// know where it is, and inventing an address would send the request to
	// whatever else happens to be listening.
	if got := c.endpointFor(CustomProvider).URL; got != "" {
		t.Errorf("custom invented an address: %q", got)
	}
	if c.EndpointConfigured(CustomProvider) {
		t.Error("a custom endpoint with no address counts as configured")
	}
}

func TestAProviderNobodyTurnedOnIsNotConfigured(t *testing.T) {
	// The trap the default address sets: every install would claim to have
	// Ollama and LM Studio running, report text and vision as covered, and then
	// fail at the first call with a connection refused from a port nothing is
	// listening on.
	empty := &Config{}
	for _, p := range OpenAICompatibleProviders {
		if empty.EndpointConfigured(p) {
			t.Errorf("%s reads as configured on a machine that never enabled it", p)
		}
		if empty.hasVisionCredential(p) {
			t.Errorf("%s counts as a vision credential unasked", p)
		}
		if empty.TextCredentialFor(p) != "" {
			t.Errorf("%s counts as a text credential unasked", p)
		}
	}
}

func TestNoEmptyAuthorizationHeaderIsSent(t *testing.T) {
	// A server on your own machine authenticates nobody; some of them refuse a
	// request that carries an empty bearer token rather than none.
	srv, _, _, auth := endpointServer(t)
	c := &Config{Endpoints: map[string]Endpoint{OllamaLocalProvider: {URL: srv.URL, Model: "m"}}}
	if _, err := c.LLMComplete(context.Background(), LLMRequest{
		Provider: OllamaLocalProvider,
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if *auth != "" {
		t.Errorf("sent Authorization: %q", *auth)
	}
}

func TestTheModelListIsWhatTheServerSays(t *testing.T) {
	// The only honest source for a model picker: what is installed is the
	// person's business and changes whenever they pull something new.
	srv, _, path, _ := endpointServer(t)
	c := &Config{Endpoints: map[string]Endpoint{CustomProvider: {URL: srv.URL}}}
	got, err := c.ModelList(context.Background(), CustomProvider)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "alpaca,zebra" {
		t.Errorf("model list: %v", got)
	}
	if *path != "/models" {
		t.Errorf("asked %q", *path)
	}
}

func TestAnAddressThatIsNotAModelServerSaysSo(t *testing.T) {
	// Typing a web page's address into the custom field is the likeliest way to
	// get this wrong, and "invalid character '<'" would not help anybody.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	t.Cleanup(srv.Close)
	c := &Config{Endpoints: map[string]Endpoint{CustomProvider: {URL: srv.URL}}}
	_, err := c.ModelList(context.Background(), CustomProvider)
	if err == nil || !strings.Contains(err.Error(), "model list") {
		t.Fatalf("expected a plain explanation, got %v", err)
	}
}

func TestTheseEndpointsNeverRunOnTheHostedService(t *testing.T) {
	// A hosted server cannot reach a laptop, and letting an account name an
	// address for our server to call would hand it a way to probe the inside of
	// our own network. They must stay on the machine the person is sitting at.
	for _, p := range OpenAICompatibleProviders {
		found := false
		for _, l := range LocalOnlyTextProviders {
			if l == p {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not local-only", p)
		}
	}
}

func TestTheThreeAreOfferedForBothJobs(t *testing.T) {
	for _, p := range OpenAICompatibleProviders {
		if !contains(TextProviders, p) {
			t.Errorf("%s missing from text", p)
		}
		if !contains(VisionProviders, p) {
			t.Errorf("%s missing from vision", p)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
