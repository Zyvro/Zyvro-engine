package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Xiaomi MiMo is the same chat API as the local endpoints, at a hosted address
// with a key. The ways that goes wrong quietly: counting it as configured with
// no key, sending the request without the key, and an empty model picker for a
// service that has no /models route.

func TestMimoWithoutAKeyIsNotConfigured(t *testing.T) {
	c := &Config{Endpoints: map[string]Endpoint{MimoProvider: {}}}
	if c.EndpointConfigured(MimoProvider) {
		t.Fatal("MiMo with no key counted as configured: every call would be a 401")
	}
	if got := c.TextCredential(MimoProvider); got != "" {
		t.Fatalf("TextCredential = %q, want empty without a key", got)
	}
	if got := c.EndpointFor(MimoProvider).URL; got != "https://api.xiaomimimo.com/v1" {
		t.Fatalf("default address = %q", got)
	}
}

func TestMimoSendsItsKeyAndDefaultModel(t *testing.T) {
	srv, seen, path, auth := endpointServer(t)
	c := &Config{Endpoints: map[string]Endpoint{MimoProvider: {URL: srv.URL + "/v1", Key: "tp-secret"}}}
	if !c.EndpointConfigured(MimoProvider) {
		t.Fatal("MiMo with an address and a key is not configured")
	}
	if got := c.TextCredential(MimoProvider); got == "" {
		t.Fatal("TextCredential is empty for a configured MiMo")
	}
	if _, err := c.LLMComplete(context.Background(), LLMRequest{Provider: MimoProvider, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if *path != "/v1/chat/completions" {
		t.Fatalf("path = %q", *path)
	}
	if *auth != "Bearer tp-secret" {
		t.Fatalf("auth = %q", *auth)
	}
	if (*seen)["model"] != MimoDefaultModel {
		t.Fatalf("model = %v, want %s", (*seen)["model"], MimoDefaultModel)
	}
}

func TestMimoOffersItsModelsWhenThereIsNoListRoute(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	c := &Config{Endpoints: map[string]Endpoint{MimoProvider: {URL: srv.URL + "/v1", Key: "sk-x"}}}
	got, err := c.ModelList(context.Background(), MimoProvider)
	if err != nil || len(got) == 0 || got[0] != MimoDefaultModel {
		t.Fatalf("ModelList = %v, %v", got, err)
	}
}

func TestMimoIsTextOnlyAndLocalOnly(t *testing.T) {
	for _, p := range VisionProviders {
		if p == MimoProvider {
			t.Fatal("MiMo offered for vision, which nobody checked")
		}
	}
	for _, p := range HostedTextProviders() {
		if p == MimoProvider {
			t.Fatal("MiMo offered on the hosted service, which has its own key handling")
		}
	}
}

func TestMimoSaysARefusedKeyInsteadOfListingModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API Key","type":"invalid_key"}}`))
	}))
	t.Cleanup(srv.Close)
	c := &Config{Endpoints: map[string]Endpoint{MimoProvider: {URL: srv.URL + "/v1", Key: "sk-wrong"}}}
	if got, err := c.ModelList(context.Background(), MimoProvider); err == nil {
		t.Fatalf("a refused key listed models: %v", got)
	}
}
