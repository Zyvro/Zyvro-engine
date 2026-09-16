package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The Black Forest Labs adapter is asynchronous: submit, poll, download. These
// tests pin all three legs against a local server, because the interesting
// failures are in the states between them and none of them can be exercised
// against the real endpoint without spending money.

// bflServer stands in for the API. It records the submit body and walks the
// task through the statuses given, answering the last one forever.
type bflServer struct {
	*httptest.Server
	submitPath string
	submitKey  string
	submitBody map[string]any
	polls      int
}

func newBFLServer(t *testing.T, statuses []string, imageBody string) *bflServer {
	t.Helper()
	s := &bflServer{}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		s.submitPath = r.URL.Path
		s.submitKey = r.Header.Get("x-key")
		_ = json.NewDecoder(r.Body).Decode(&s.submitBody)
		fmt.Fprintf(w, `{"id":"task-1","polling_url":%q}`, s.URL+"/v1/get_result?id=task-1")
	})
	mux.HandleFunc("/v1/get_result", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-key") == "" {
			http.Error(w, "no key on the poll", http.StatusUnauthorized)
			return
		}
		i := s.polls
		s.polls++
		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		status := statuses[i]
		if status == "Ready" {
			fmt.Fprintf(w, `{"id":"task-1","status":"Ready","result":{"sample":%q}}`, s.URL+"/delivery/image.png")
			return
		}
		fmt.Fprintf(w, `{"id":"task-1","status":%q}`, status)
	})
	mux.HandleFunc("/delivery/image.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, imageBody)
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func bflConfig(url string) *Config {
	return &Config{BFLAPIKey: "test-key", BFLBaseURL: url}
}

func TestBFLSubmitsPollsAndDownloads(t *testing.T) {
	srv := newBFLServer(t, []string{"Pending", "Generating", "Ready"}, "PNGBYTES")
	c := bflConfig(srv.URL)

	img, err := c.bflImageGenerate(context.Background(), "flux-2-klein-9b", "a red potion", "1:1", "1K", nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if string(img.Data) != "PNGBYTES" {
		t.Fatalf("data = %q", img.Data)
	}
	if img.MimeType != "image/png" {
		t.Fatalf("mime = %q", img.MimeType)
	}
	// The image is downloaded rather than handed on as a link: the vendor's
	// url dies in ten minutes and a workflow can outlive that.
	if srv.polls < 3 {
		t.Fatalf("gave up polling after %d attempts", srv.polls)
	}
	if srv.submitPath != "/v1/flux-2-klein-9b" {
		t.Fatalf("submit path = %q", srv.submitPath)
	}
	if srv.submitKey != "test-key" {
		t.Fatalf("the key did not travel on the submit: %q", srv.submitKey)
	}
	if srv.submitBody["prompt"] != "a red potion" {
		t.Fatalf("body = %+v", srv.submitBody)
	}
}

// The slug becomes a path segment and it comes out of a graph.
func TestBFLRefusesAModelItDoesNotKnow(t *testing.T) {
	srv := newBFLServer(t, []string{"Ready"}, "x")
	c := bflConfig(srv.URL)

	for _, model := range []string{"../../admin", "flux-pro-1.1", "flux-2-klein-9b/../secret"} {
		if _, err := c.bflImageGenerate(context.Background(), model, "p", "1:1", "1K", nil); err == nil {
			t.Fatalf("%q was accepted as a model", model)
		}
	}
	if srv.submitPath != "" {
		t.Fatalf("a refused model still reached the network: %q", srv.submitPath)
	}
}

func TestBFLSendsReferencesAsRawBase64(t *testing.T) {
	srv := newBFLServer(t, []string{"Ready"}, "x")
	c := bflConfig(srv.URL)

	refs := []ImageResult{
		{Data: []byte("one"), MimeType: "image/png"},
		{Data: []byte("two"), MimeType: "image/png"},
	}
	if _, err := c.bflImageGenerate(context.Background(), "", "p", "1:1", "1K", refs); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got := srv.submitBody["input_image"]; got != base64.StdEncoding.EncodeToString([]byte("one")) {
		t.Fatalf("input_image = %v", got)
	}
	if got := srv.submitBody["input_image_2"]; got != base64.StdEncoding.EncodeToString([]byte("two")) {
		t.Fatalf("input_image_2 = %v", got)
	}
	// A data URL prefix here would be sent verbatim and rejected by the API.
	if s, _ := srv.submitBody["input_image"].(string); strings.HasPrefix(s, "data:") {
		t.Error("the reference went out as a data URL")
	}
}

// A refusal is not a transient state to keep polling through, and the message
// has to say which kind it was.
func TestBFLStopsOnModeration(t *testing.T) {
	for _, status := range []string{"Request Moderated", "Content Moderated"} {
		srv := newBFLServer(t, []string{status}, "x")
		_, err := bflConfig(srv.URL).bflImageGenerate(context.Background(), "", "p", "1:1", "1K", nil)
		if err == nil {
			t.Fatalf("%s was not reported as a failure", status)
		}
		if !strings.Contains(err.Error(), status) {
			t.Errorf("error does not name the status: %v", err)
		}
		if srv.polls > 1 {
			t.Errorf("%s kept polling %d times", status, srv.polls)
		}
	}
}

// A run already has a deadline. Polling must honour it rather than wait out a
// task that will never finish.
func TestBFLGivesUpWithTheContext(t *testing.T) {
	srv := newBFLServer(t, []string{"Pending"}, "x")
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := bflConfig(srv.URL).bflImageGenerate(ctx, "", "p", "1:1", "1K", nil)
	if err == nil {
		t.Fatal("polling a task that never finishes returned no error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("gave up after %s, long past the deadline", time.Since(start))
	}
}

func TestBFLWithoutAKeyIsRefusedBeforeAnyRequest(t *testing.T) {
	srv := newBFLServer(t, []string{"Ready"}, "x")
	c := &Config{BFLBaseURL: srv.URL}
	if _, err := c.bflImageGenerate(context.Background(), "", "p", "1:1", "1K", nil); err == nil {
		t.Fatal("a missing key was not reported")
	}
	if srv.submitPath != "" {
		t.Error("a keyless request still reached the network")
	}
}

func TestBFLDimensions(t *testing.T) {
	for _, tc := range []struct {
		aspect, size string
		w, h         int
	}{
		{"1:1", "1K", 1024, 1024},
		{"16:9", "1K", 1024, 576},
		{"9:16", "1K", 576, 1024},
		{"1:1", "2K", 1536, 1536},
		{"3:2", "1K", 1024, 672},
		{"", "", 1024, 1024},
	} {
		w, h := bflDimensions(tc.aspect, tc.size)
		if w != tc.w || h != tc.h {
			t.Errorf("%q %q -> %dx%d, want %dx%d", tc.aspect, tc.size, w, h, tc.w, tc.h)
		}
		if w%32 != 0 || h%32 != 0 {
			t.Errorf("%q %q -> %dx%d, not multiples of 32", tc.aspect, tc.size, w, h)
		}
	}
}

// ---------- the router ----------

func TestImageProviderFollowsTheCredentialWhenNobodyChose(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       *Config
		want      string
		requested string
	}{
		{name: "only the platform's Black Forest key", cfg: &Config{BFLAPIKey: "k"}, want: "bfl"},
		{name: "the user brought a Google key", cfg: &Config{GoogleAPIKey: "g"}, want: "google"},
		{name: "both, nobody chose", cfg: &Config{GoogleAPIKey: "g", BFLAPIKey: "k"}, want: "google"},
		{name: "the node chose", cfg: &Config{GoogleAPIKey: "g", BFLAPIKey: "k"}, requested: "klein", want: "bfl"},
		{name: "the deployment chose", cfg: &Config{GoogleAPIKey: "g", BFLAPIKey: "k", ImageProvider: "bfl"}, want: "bfl"},
		{name: "neither", cfg: &Config{}, want: "google"},
	} {
		if got := tc.cfg.resolveImageProvider(tc.requested); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A Gemini model name means nothing to Black Forest Labs. Routing there with
// one must not send it.
func TestRoutingToBFLDropsAModelThatIsNotItsOwn(t *testing.T) {
	srv := newBFLServer(t, []string{"Ready"}, "x")
	c := bflConfig(srv.URL)

	if _, err := c.ImageGenerate(context.Background(), "bfl", "gemini-3.1-flash-image", "p", "1:1", "1K", nil); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if srv.submitPath != "/v1/"+BFLDefaultModel {
		t.Fatalf("submit path = %q, want the Black Forest default", srv.submitPath)
	}
}
