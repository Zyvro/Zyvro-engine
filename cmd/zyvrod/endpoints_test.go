package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/providers"
)

// The local endpoints are the first providers configured by an address instead
// of a key, and that difference is where they can go wrong: the panel has to
// draw them differently, the file has to keep them alongside the ordering, and
// the engine has to actually receive them. Each of those was a separate place
// something could be saved and never read.

func decodeCatalog(t *testing.T, body []byte) map[string]providerInfo {
	t.Helper()
	var parsed struct {
		Providers []providerInfo `json:"providers"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := map[string]providerInfo{}
	for _, p := range parsed.Providers {
		out[p.ID] = p
	}
	return out
}

func TestTheLocalEndpointsAreOfferedForTextAndVision(t *testing.T) {
	e := newTestEnv(t)
	got := decodeCatalog(t, e.do("GET", "/api/providers", nil).Body.Bytes())
	for _, id := range providers.AddressConfiguredProviders {
		p, ok := got[id]
		if !ok {
			t.Fatalf("%s missing from the catalogue", id)
		}
		if !p.Endpoint {
			t.Errorf("%s is not marked as an address provider, so the panel would draw a key box", id)
		}
		want := "text,vision"
		if id == providers.CustomImageProvider {
			// The images half of the same API: a different shape, so a
			// different job, and offering it for text would be the "you are
			// covered" lie the vision split was fixed to stop telling.
			want = "image"
		}
		if strings.Join(p.Roles, ",") != want {
			t.Errorf("%s roles: %v", id, p.Roles)
		}
		if p.HasUserKey {
			t.Errorf("%s reads as configured before anybody enabled it", id)
		}
		if p.DefaultURL != providers.DefaultEndpointURL(id) {
			t.Errorf("%s default address: %q", id, p.DefaultURL)
		}
	}
}

func TestSavingAnEndpointReachesTheEngine(t *testing.T) {
	// The failure this rules out is the one the ordering already had: saved to
	// the file, shown in the panel, and never put into the config a run uses.
	e := newTestEnv(t)
	res := e.do("PUT", "/api/providers/lmstudio/endpoint", map[string]string{
		"url": "http://127.0.0.1:1234/v1", "model": "qwen/qwen3-coder-next",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("save: %d %s", res.Code, res.Body)
	}
	got := decodeCatalog(t, res.Body.Bytes())["lmstudio"]
	if got.EndpointURL != "http://127.0.0.1:1234/v1" || got.Model != "qwen/qwen3-coder-next" {
		t.Errorf("catalogue: %+v", got)
	}
	if !got.HasUserKey {
		t.Error("a configured endpoint still reads as unconfigured")
	}

	cfg := e.daemon.providerConfig()
	if cfg.EndpointFor("lmstudio").Model != "qwen/qwen3-coder-next" {
		t.Errorf("the engine never received it: %+v", cfg.EndpointFor("lmstudio"))
	}
	if cfg.ResolvedVisionProvider("lmstudio") != "lmstudio" {
		t.Error("a vision call naming lmstudio would not land there")
	}
}

func TestTheChosenOrderReachesTheEngine(t *testing.T) {
	// Same hole, found while wiring the endpoints: the panel let somebody put
	// their local server first for vision, saved it, and runs kept going to
	// whichever credential happened to exist.
	e := newTestEnv(t)
	if res := e.do("PUT", "/api/providers/ollama-local/endpoint", map[string]string{
		"url": "http://127.0.0.1:11434/v1", "model": "gemma3:4b",
	}); res.Code != http.StatusOK {
		t.Fatalf("save endpoint: %d %s", res.Code, res.Body)
	}
	if res := e.do("PUT", "/api/providers/order", map[string]any{
		"order": map[string][]string{"vision": {"ollama-local", "google"}},
	}); res.Code != http.StatusOK {
		t.Fatalf("save order: %d %s", res.Code, res.Body)
	}

	cfg := e.daemon.providerConfig()
	cfg.GoogleAPIKey = "AIzaPretend"
	if got := cfg.ResolvedVisionProvider(""); got != "ollama-local" {
		t.Errorf("a vision call with no named provider went to %q despite the order", got)
	}
}

func TestClearingAnEndpointTurnsItOff(t *testing.T) {
	// These have no key to delete, so an empty address is how "forget this one"
	// is spelled.
	e := newTestEnv(t)
	e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": "http://127.0.0.1:9000/v1", "model": "klein-2.0"})
	res := e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": "", "model": ""})
	if res.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", res.Code, res.Body)
	}
	if got := decodeCatalog(t, res.Body.Bytes())["custom"]; got.HasUserKey || got.EndpointURL != "" {
		t.Errorf("still configured: %+v", got)
	}
	if e.daemon.providerConfig().EndpointConfigured("custom") {
		t.Error("the engine still holds the cleared endpoint")
	}
}

func TestAnAddressWithoutASchemeIsRefusedUpFront(t *testing.T) {
	// A bare host:port is the likeliest thing to type, and letting it through
	// produces a transport error about a missing scheme at the first run rather
	// than about the box that needs one.
	e := newTestEnv(t)
	res := e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": "127.0.0.1:9000/v1"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected a refusal, got %d %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "http://") {
		t.Errorf("the message does not say what is missing: %s", res.Body)
	}
}

func TestOnlyAddressProvidersHaveAnEndpointAndAModelList(t *testing.T) {
	e := newTestEnv(t)
	if res := e.do("PUT", "/api/providers/google/endpoint", map[string]string{"url": "http://evil.example/v1"}); res.Code != http.StatusBadRequest {
		t.Errorf("google accepted an address: %d", res.Code)
	}
	if res := e.do("GET", "/api/providers/anthropic/models", nil); res.Code != http.StatusBadRequest {
		t.Errorf("anthropic offered a model list: %d", res.Code)
	}
}

func TestTheModelListIsWhateverTheServerAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gemma3:4b"},{"id":"llama3.1:latest"}]}`))
	}))
	t.Cleanup(srv.Close)

	e := newTestEnv(t)
	e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": srv.URL})
	res := e.do("GET", "/api/providers/custom/models", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("models: %d %s", res.Code, res.Body)
	}
	var parsed struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if strings.Join(parsed.Models, ",") != "gemma3:4b,llama3.1:latest" {
		t.Errorf("models: %v", parsed.Models)
	}
}

func TestAServerThatIsNotRunningSaysSoRatherThanFailingBlankly(t *testing.T) {
	e := newTestEnv(t)
	e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": "http://127.0.0.1:9/v1"})
	res := e.do("GET", "/api/providers/custom/models", nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("expected a gateway failure, got %d %s", res.Code, res.Body)
	}
	if res.Body.Len() == 0 {
		t.Error("no explanation at all")
	}
}
