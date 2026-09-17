package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/providers"
)

// Editing used to be Gemini's alone, on the reasoning that it had no equivalent
// elsewhere. It has: an image and a prompt in, an image out, is what FLUX does
// and what a diffusion server on somebody's own machine does. These tests are
// about the two ways that can still go wrong — landing on the wrong backend,
// and arriving there carrying another backend's model name.

type recordedImageCall struct {
	path  string
	model string
}

func newFakeImageEndpoint(t *testing.T, seen *[]recordedImageCall) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := recordedImageCall{path: r.URL.Path}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			_ = r.ParseMultipartForm(8 << 20)
			call.model = r.FormValue("model")
		} else {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if m, ok := body["model"].(string); ok {
				call.model = m
			}
		}
		*seen = append(*seen, call)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` +
			base64.StdEncoding.EncodeToString(solidWithBorder(16, 16, red, red)) + `"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runWithImageEndpoint(t *testing.T, g *Graph, endpoint string) (*Runtime, error) {
	t.Helper()
	cfg := &providers.Config{
		// A Gemini base that would fail if anything reached it, so a call
		// landing on the wrong backend fails loudly rather than quietly.
		GeminiBaseURL: "http://127.0.0.1:9",
		GoogleAPIKey:  "test-key-not-a-real-one",
		ImageModel:    "gemini-image-model",
		Endpoints: map[string]providers.Endpoint{
			providers.CustomImageProvider: {URL: endpoint + "/v1", Model: "the-endpoint-model"},
		},
	}
	rt := NewRuntime("exec-fixed", g, cfg, &recordingStore{}, nil)
	return rt, rt.Execute(context.Background())
}

func TestEditingCanRunOnALocalImageServer(t *testing.T) {
	var seen []recordedImageCall
	srv := newFakeImageEndpoint(t, &seen)

	_, err := runWithImageEndpoint(t, &Graph{
		Nodes: []GraphNode{
			imageSource("A", solidWithBorder(16, 16, red, red)),
			node("B", "editImage", map[string]any{"prompt": "make it warmer", "provider": "custom-image"}),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "B")},
	}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("calls: %+v", seen)
	}
	if seen[0].path != "/v1/images/edits" {
		// An edit is a generation with the source as its reference, and the
		// protocol sends references as multipart to the edits route.
		t.Errorf("an edit went to %q", seen[0].path)
	}
	if seen[0].model != "the-endpoint-model" {
		// gemini-image-model reaching this server would be the failure that
		// reads as "no such model" and sends the reader looking at the server.
		t.Errorf("model sent: %q", seen[0].model)
	}
}

func TestGeneratingOnALocalServerNeverCarriesTheGeminiModelName(t *testing.T) {
	var seen []recordedImageCall
	srv := newFakeImageEndpoint(t, &seen)

	_, err := runWithImageEndpoint(t, &Graph{
		Nodes: []GraphNode{
			node("A", "textInput", map[string]any{"value": "a red fox"}),
			node("B", "generateImage", map[string]any{"provider": "custom-image"}),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "B")},
	}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].path != "/v1/images/generations" {
		t.Fatalf("calls: %+v", seen)
	}
	if seen[0].model != "the-endpoint-model" {
		t.Errorf("model sent: %q", seen[0].model)
	}
}

func TestANodesOwnModelStillWins(t *testing.T) {
	var seen []recordedImageCall
	srv := newFakeImageEndpoint(t, &seen)

	_, err := runWithImageEndpoint(t, &Graph{
		Nodes: []GraphNode{
			node("A", "textInput", map[string]any{"value": "a red fox"}),
			node("B", "generateImage", map[string]any{"provider": "custom-image", "model": "a-model-this-node-named"}),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "B")},
	}, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].model != "a-model-this-node-named" {
		t.Fatalf("calls: %+v", seen)
	}
}
