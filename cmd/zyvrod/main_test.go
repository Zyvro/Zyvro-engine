package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zyvro/Zyvro-engine/localstore"
)

// providerFreeGraph is a textInput feeding an output node. It exercises the
// whole run path — reporter, run file, polling — without touching a model, so
// the tests never need a provider key or a network.
const providerFreeGraph = `{
  "nodes": [
    {"id":"n1","type":"textInput","position":{"x":0,"y":0},"data":{"label":"Prompt","config":{"value":"hello from the daemon"}}},
    {"id":"n2","type":"output","position":{"x":300,"y":0},"data":{"config":{}}}
  ],
  "edges": [
    {"id":"e1","source":"n1","target":"n2","sourceHandle":"out","targetHandle":"in","type":"data"}
  ]
}`

type testEnv struct {
	t       *testing.T
	daemon  *daemon
	handler http.Handler
	store   *localstore.Store
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	store, err := localstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	d, err := newDaemon(store)
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	return &testEnv{t: t, daemon: d, handler: d.handler(), store: store}
}

// do sends an authenticated request, which is what every client of this daemon
// looks like once it has read the handshake line.
func (e *testEnv) do(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.raw(method, path, body, map[string]string{"Authorization": "Bearer " + e.daemon.token})
}

func (e *testEnv) raw(method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

// /health is the liveness probe Electron uses before it has anything else, so
// it must answer without a token and say nothing more than that we are up.
func TestHealthNeedsNoToken(t *testing.T) {
	e := newTestEnv(t)
	rec := e.raw(http.MethodGet, "/health", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %q", got)
	}
}

// Without the token nothing under /api answers. This is the whole defence
// against another process, or a page in a browser, driving the daemon.
func TestAPIRequiresToken(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/workflows"},
		{http.MethodPost, "/api/workflows"},
		{http.MethodGet, "/api/workflows/" + localstore.NewID()},
		{http.MethodPut, "/api/workflows/" + localstore.NewID()},
		{http.MethodDelete, "/api/workflows/" + localstore.NewID()},
		{http.MethodPost, "/api/executions"},
		{http.MethodGet, "/api/executions/" + localstore.NewID()},
		{http.MethodGet, "/api/providers"},
		{http.MethodGet, "/api/secrets"},
		{http.MethodPut, "/api/secrets"},
		{http.MethodDelete, "/api/secrets/anthropic"},
		{http.MethodGet, "/api/auth/me"},
		{http.MethodGet, "/api/ai/quota"},
		{http.MethodGet, "/api/local/status"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			if rec := e.raw(c.method, c.path, nil, nil); rec.Code != http.StatusUnauthorized {
				t.Errorf("no header: status = %d, want 401", rec.Code)
			}
			wrong := map[string]string{"Authorization": "Bearer " + localstore.NewID()}
			if rec := e.raw(c.method, c.path, nil, wrong); rec.Code != http.StatusUnauthorized {
				t.Errorf("wrong token: status = %d, want 401", rec.Code)
			}
			raw := map[string]string{"Authorization": e.daemon.token}
			if rec := e.raw(c.method, c.path, nil, raw); rec.Code != http.StatusUnauthorized {
				t.Errorf("token without the Bearer scheme: status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAPIAcceptsToken(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(http.MethodGet, "/api/workflows", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := decode[[]localstore.Workflow](t, rec); len(got) != 0 {
		t.Errorf("a fresh project listed %d workflows", len(got))
	}
}

func TestWorkflowCRUDRoundTrip(t *testing.T) {
	e := newTestEnv(t)

	created := decode[localstore.Workflow](t, mustStatus(t,
		e.do(http.MethodPost, "/api/workflows", map[string]any{
			"name":       "Round Trip",
			"graph_json": json.RawMessage(providerFreeGraph),
		}), http.StatusCreated))
	if !localstore.ValidID(created.ID) {
		t.Fatalf("created id = %q", created.ID)
	}

	list := decode[[]localstore.Workflow](t, mustStatus(t, e.do(http.MethodGet, "/api/workflows", nil), http.StatusOK))
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v", list)
	}

	got := decode[localstore.Workflow](t, mustStatus(t, e.do(http.MethodGet, "/api/workflows/"+created.ID, nil), http.StatusOK))
	if got.Name != "Round Trip" {
		t.Errorf("name = %q", got.Name)
	}

	updated := decode[localstore.Workflow](t, mustStatus(t,
		e.do(http.MethodPut, "/api/workflows/"+created.ID, map[string]any{
			"name":        "Round Trip v2",
			"description": "edited",
		}), http.StatusOK))
	if updated.Name != "Round Trip v2" || updated.Description != "edited" || updated.Version != 2 {
		t.Errorf("update = %+v", updated)
	}
	// The graph was not in the patch, so it must survive untouched.
	if !strings.Contains(updated.GraphJSON, "textInput") {
		t.Errorf("update dropped the graph: %q", updated.GraphJSON)
	}

	mustStatus(t, e.do(http.MethodDelete, "/api/workflows/"+created.ID, nil), http.StatusOK)
	mustStatus(t, e.do(http.MethodGet, "/api/workflows/"+created.ID, nil), http.StatusNotFound)
}

// An id from a URL is never a path. A rejected id and a missing one both read
// as 404 so the API cannot be used to probe the filesystem. A bare ".." never
// reaches a handler at all: the mux normalizes it away and redirects, which is
// also a refusal.
//
// The assertion is the property, not a status code. Go changed the code the mux
// uses for that redirect (301 became a method-preserving 307), and a test that
// enumerates codes fails on a toolchain bump while the behaviour is unchanged.
// What has to hold is that nothing is ever served, and that a redirect cannot
// land anywhere that would serve something.
func TestWorkflowIDsAreValidated(t *testing.T) {
	e := newTestEnv(t)
	for _, id := range []string{"..", "not-an-id", "0123456789abcdef0123456", "%2e%2e%2f%2e%2e%2fetc%2fpasswd"} {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			rec := e.do(method, "/api/workflows/"+id, nil)

			if rec.Code == http.StatusOK {
				t.Errorf("%s id=%q was served: %s", method, id, rec.Body.String())
				continue
			}

			switch {
			case rec.Code == http.StatusNotFound:
				// The handler saw the id and refused it.
			case rec.Code >= 300 && rec.Code < 400:
				// The mux normalized the path away. Follow it once: the target
				// must not be a workflow the caller could not otherwise reach.
				location := rec.Header().Get("Location")
				if strings.HasPrefix(location, "/api/workflows/") {
					t.Errorf("%s id=%q redirects back into the workflow routes: %q", method, id, location)
					continue
				}
				if followed := e.do(method, location, nil); followed.Code == http.StatusOK {
					t.Errorf("%s id=%q redirects to %q, which serves: %s", method, id, location, followed.Body.String())
				}
			default:
				t.Errorf("%s id=%q status = %d, want a 404 or a redirect", method, id, rec.Code)
			}
		}
	}
}

// The reused Builder component asks who it is talking to before it renders.
// There is no account here, so the answer is a synthetic local user rather than
// a redirect to a login page the desktop app does not have.
func TestAuthMeReturnsLocalUser(t *testing.T) {
	e := newTestEnv(t)
	user := decode[struct {
		ID      string `json:"id"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		IsAdmin bool   `json:"is_admin"`
	}](t, mustStatus(t, e.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK))

	if user.ID != "local" || user.Email != "local@zyvro" || user.IsAdmin {
		t.Errorf("auth/me = %+v", user)
	}
	if user.Name != filepath.Base(e.store.Root) {
		t.Errorf("name = %q, want the project folder name %q", user.Name, filepath.Base(e.store.Root))
	}
}

func TestDuplicateWorkflow(t *testing.T) {
	e := newTestEnv(t)
	src := decode[localstore.Workflow](t, mustStatus(t,
		e.do(http.MethodPost, "/api/workflows", map[string]any{
			"name":       "Original",
			"graph_json": json.RawMessage(providerFreeGraph),
		}), http.StatusCreated))

	copied := decode[localstore.Workflow](t, mustStatus(t,
		e.do(http.MethodPost, "/api/workflows/"+src.ID+"/duplicate", nil), http.StatusCreated))
	if copied.ID == src.ID {
		t.Fatal("duplicate reused the original's id")
	}
	if !strings.Contains(copied.Name, "Original") || copied.Version != 1 {
		t.Errorf("copy = %+v", copied)
	}
	if copied.GraphJSON != src.GraphJSON {
		t.Errorf("copy did not carry the graph")
	}

	after := decode[localstore.Workflow](t, mustStatus(t, e.do(http.MethodGet, "/api/workflows/"+src.ID, nil), http.StatusOK))
	if after.DuplicateCount != 1 {
		t.Errorf("duplicate_count = %d, want 1", after.DuplicateCount)
	}
	mustStatus(t, e.do(http.MethodPost, "/api/workflows/"+localstore.NewID()+"/duplicate", nil), http.StatusNotFound)
}

// The settings panel stores keys through these three routes. A stored key must
// never come back out over the API, and the file it lands in must be private.
func TestSecretsRoundTrip(t *testing.T) {
	e := newTestEnv(t)

	if got := decode[[]localstore.Secret](t, mustStatus(t, e.do(http.MethodGet, "/api/secrets", nil), http.StatusOK)); len(got) != 0 {
		t.Fatalf("a fresh project already has secrets: %+v", got)
	}

	const key = "sk-ant-api-secret-value-9876"
	set := decode[map[string]any](t, mustStatus(t,
		e.do(http.MethodPut, "/api/secrets", map[string]any{"provider": "anthropic", "secret": key}), http.StatusOK))
	if set["secret_last4"] != "9876" {
		t.Errorf("secret_last4 = %v", set["secret_last4"])
	}
	if body := mustStatus(t, e.do(http.MethodGet, "/api/secrets", nil), http.StatusOK).Body.String(); strings.Contains(body, key) {
		t.Fatal("the listing echoed the secret back")
	}

	list := decode[[]localstore.Secret](t, mustStatus(t, e.do(http.MethodGet, "/api/secrets", nil), http.StatusOK))
	if len(list) != 1 || list[0].Provider != "anthropic" || list[0].SecretLast4 != "9876" {
		t.Fatalf("list = %+v", list)
	}

	// A stored key is what the next run uses, ahead of the environment.
	if got := e.daemon.providerConfig().AnthropicAPIKey; got != key {
		t.Errorf("providerConfig did not pick up the stored key (got %q)", got)
	}
	listing := decode[struct {
		Providers []providerInfo      `json:"providers"`
		Order     map[string][]string `json:"order"`
	}](t, mustStatus(t, e.do(http.MethodGet, "/api/providers", nil), http.StatusOK))
	catalog := listing.Providers
	for _, p := range catalog {
		if p.ID == "anthropic" && (!p.HasUserKey || p.UserKeyLast4 != "9876") {
			t.Errorf("catalog did not reflect the stored key: %+v", p)
		}
	}

	mustStatus(t, e.do(http.MethodDelete, "/api/secrets/anthropic", nil), http.StatusOK)
	if got := decode[[]localstore.Secret](t, mustStatus(t, e.do(http.MethodGet, "/api/secrets", nil), http.StatusOK)); len(got) != 0 {
		t.Errorf("secret survived deletion: %+v", got)
	}
}

func TestSecretsRejectUnknownProviders(t *testing.T) {
	e := newTestEnv(t)
	for _, provider := range []string{"", "nope", "../../etc/passwd"} {
		rec := e.do(http.MethodPut, "/api/secrets", map[string]any{"provider": provider, "secret": "x"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("provider %q: status = %d, want 400", provider, rec.Code)
		}
	}
	// The CLI providers are configured by being installed, so there is nothing
	// to store for them and pretending otherwise would be misleading.
	rec := e.do(http.MethodPut, "/api/secrets", map[string]any{"provider": "claude-cli", "secret": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("claude-cli: status = %d, want 400", rec.Code)
	}
}

// The secrets file is plaintext by design, which makes its permissions and the
// gitignore the two things that stop a key leaking.
func TestSecretsFileIsPrivateAndIgnored(t *testing.T) {
	e := newTestEnv(t)
	mustStatus(t, e.do(http.MethodPut, "/api/secrets", map[string]any{"provider": "openai", "secret": "sk-test-1234"}), http.StatusOK)

	info, err := os.Stat(filepath.Join(e.store.ZyvroDir(), "secrets.json"))
	if err != nil {
		t.Fatalf("secrets.json missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets.json mode = %o, want 600", perm)
	}

	ignore, err := os.ReadFile(filepath.Join(e.store.ZyvroDir(), ".gitignore"))
	if err != nil {
		t.Fatalf(".zyvro/.gitignore missing: %v", err)
	}
	if !strings.Contains(string(ignore), "secrets.json") {
		t.Errorf(".gitignore does not exclude secrets.json:\n%s", ignore)
	}
}

// The free description writer is funded by the hosted platform key. There is no
// platform key locally, so the quota endpoint says "off" rather than quietly
// spending the user's own credits.
func TestAiQuotaIsOff(t *testing.T) {
	e := newTestEnv(t)
	quota := decode[struct {
		Allowed           bool `json:"allowed"`
		RetryAfterSeconds int  `json:"retry_after_seconds"`
		WindowSeconds     int  `json:"window_seconds"`
	}](t, mustStatus(t, e.do(http.MethodGet, "/api/ai/quota", nil), http.StatusOK))
	if quota.Allowed {
		t.Errorf("quota = %+v, want allowed:false", quota)
	}
}

// A run's provider config comes from the project, not only the environment:
// the CLI runs where the user's files are, and the project decides which text
// provider a node uses when it names none.
func TestProviderConfigUsesProjectDefaults(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.daemon.providerConfig()
	if cfg.LocalCLIWorkdir != e.store.Root {
		t.Errorf("LocalCLIWorkdir = %q, want the project folder %q", cfg.LocalCLIWorkdir, e.store.Root)
	}
	if cfg.TextProvider != localstore.DefaultTextProvider {
		t.Errorf("TextProvider = %q, want %q", cfg.TextProvider, localstore.DefaultTextProvider)
	}
}

// A full run: the execution is created immediately, progress lands in the run
// file, and polling sees it complete — the same sequence the builder drives.
func TestExecutionRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	wf := decode[localstore.Workflow](t, mustStatus(t,
		e.do(http.MethodPost, "/api/workflows", map[string]any{
			"name":       "Runnable",
			"graph_json": json.RawMessage(providerFreeGraph),
		}), http.StatusCreated))

	start := decode[struct {
		ExecutionID string `json:"execution_id"`
		Status      string `json:"status"`
	}](t, mustStatus(t, e.do(http.MethodPost, "/api/executions", map[string]any{
		"workflow_id": wf.ID,
		"inputs":      map[string]any{},
	}), http.StatusAccepted))
	if !localstore.ValidID(start.ExecutionID) {
		t.Fatalf("execution_id = %q", start.ExecutionID)
	}

	final := e.waitForExecution(start.ExecutionID)
	if final.Execution.Status != "completed" {
		t.Fatalf("status = %q, error = %q", final.Execution.Status, final.Execution.Error)
	}
	if !strings.Contains(final.Execution.OutputJSON, "hello from the daemon") {
		t.Errorf("output_json = %q", final.Execution.OutputJSON)
	}
	if len(final.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(final.Nodes))
	}
	for _, n := range final.Nodes {
		if n.Status != "completed" && n.Status != "cached" {
			t.Errorf("node %s status = %q (%s)", n.NodeID, n.Status, n.Error)
		}
		if n.ExecutionID != start.ExecutionID {
			t.Errorf("node %s belongs to execution %q", n.NodeID, n.ExecutionID)
		}
	}

	// The history routes see the same run.
	execs := decode[[]localstore.Execution](t, mustStatus(t,
		e.do(http.MethodGet, "/api/workflows/"+wf.ID+"/executions", nil), http.StatusOK))
	if len(execs) != 1 || execs[0].ID != start.ExecutionID {
		t.Fatalf("executions = %+v", execs)
	}
	last := decode[executionEnvelope](t, mustStatus(t,
		e.do(http.MethodGet, "/api/workflows/"+wf.ID+"/executions/last", nil), http.StatusOK))
	if last.Execution.ID != start.ExecutionID || len(last.Nodes) != 2 {
		t.Fatalf("last execution = %+v", last)
	}

	// And the run is on disk, where the user can read it.
	runFile := filepath.Join(e.store.ZyvroDir(), "runs", start.ExecutionID+".json")
	if _, err := os.Stat(runFile); err != nil {
		t.Errorf("run file missing: %v", err)
	}
}

// A workflow with no last run answers with an empty envelope rather than a
// 404, because the builder asks for it on every page load.
func TestLastExecutionEmpty(t *testing.T) {
	e := newTestEnv(t)
	wf := decode[localstore.Workflow](t, mustStatus(t,
		e.do(http.MethodPost, "/api/workflows", map[string]any{"name": "Fresh"}), http.StatusCreated))
	rec := mustStatus(t, e.do(http.MethodGet, "/api/workflows/"+wf.ID+"/executions/last", nil), http.StatusOK)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["execution"] != nil {
		t.Errorf("execution = %v, want null", body["execution"])
	}
}

func TestRunExecutionRejectsUnknownWorkflow(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(http.MethodPost, "/api/executions", map[string]any{"workflow_id": localstore.NewID()})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	rec = e.do(http.MethodPost, "/api/executions", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing workflow_id: status = %d, want 400", rec.Code)
	}
}

// /content serves the media directory and nothing else. A traversal here would
// hand any file on the user's disk to whatever can reach the port.
func TestContentRefusesTraversal(t *testing.T) {
	e := newTestEnv(t)

	secret := filepath.Join(e.store.Root, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	outside := filepath.Join(e.store.ZyvroDir(), "project.json")
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("project.json missing: %v", err)
	}

	for _, path := range []string{
		"/content/..%2f..%2fsecret.txt",
		"/content/..%2fproject.json",
		"/content/%2e%2e%2f%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/content/../../secret.txt",
		"/content/../project.json",
		"/content/",
	} {
		rec := e.raw(http.MethodGet, path, nil, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("%s was served: %q", path, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "top secret") {
			t.Errorf("%s leaked the file outside the media dir", path)
		}
	}
}

// The legitimate case still works: a file the media store wrote is served back
// under the URL it handed out.
func TestContentServesMedia(t *testing.T) {
	e := newTestEnv(t)
	url, err := e.store.Media().SaveMedia(localstore.NewID(), "n1", "out.png", []byte("png-bytes"), "image/png")
	if err != nil {
		t.Fatalf("SaveMedia: %v", err)
	}
	rec := e.raw(http.MethodGet, url, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "png-bytes" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestProvidersCatalogShape(t *testing.T) {
	e := newTestEnv(t)
	// The listing carries the project's own order beside the catalogue, so the
	// panel can show the list the way runs will actually use it.
	listing := decode[struct {
		Providers []providerInfo      `json:"providers"`
		Order     map[string][]string `json:"order"`
	}](t, mustStatus(t, e.do(http.MethodGet, "/api/providers", nil), http.StatusOK))
	catalog := listing.Providers

	byID := map[string]providerInfo{}
	for _, p := range catalog {
		byID[p.ID] = p
		if p.Label == "" || p.Purpose == "" {
			t.Errorf("provider %s is missing display text: %+v", p.ID, p)
		}
		// A provider does more than one job: Gemini generates images and reads
		// them, Ollama writes text and reads images. A provider with no job at
		// all would be one the panel shows and nothing can use.
		if len(p.Roles) == 0 {
			t.Errorf("provider %s has no role", p.ID)
		}
		for _, role := range p.Roles {
			if role != "text" && role != "image" && role != "vision" {
				t.Errorf("provider %s has role %q", p.ID, role)
			}
		}
	}
	// Black Forest Labs was missing from this catalogue entirely, so the desktop
	// could not use a backend the engine has driven since it was added — found
	// when splitting image from vision made the image section read as one
	// provider with no alternative.
	for _, want := range []string{"google", "bfl", "anthropic", "openai", "ollama", "claude-cli", "codex-cli"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("catalog is missing %q", want)
		}
	}
	// The three jobs are genuinely different, and the catalogue has to say so.
	// Folding vision into image was what told somebody holding an Ollama key
	// they needed a Google one to read a screenshot.
	covers := func(id, role string) bool {
		for _, r := range byID[id].Roles {
			if r == role {
				return true
			}
		}
		return false
	}
	if !covers("google", "image") || !covers("google", "vision") {
		t.Error("Google does both image generation and vision, and the catalogue should say so")
	}
	if !covers("ollama", "text") || !covers("ollama", "vision") {
		t.Error("Ollama writes text and reads images, and lost one of those")
	}
	if covers("ollama", "image") {
		t.Error("Ollama was listed for image generation, which it cannot do")
	}
	if !covers("bfl", "image") || covers("bfl", "vision") || covers("bfl", "text") {
		t.Errorf("Black Forest Labs generates images and does nothing else: %v", byID["bfl"].Roles)
	}
	for _, id := range []string{"claude-cli", "codex-cli"} {
		if covers(id, "vision") || covers(id, "image") {
			t.Errorf("%s was listed for a job it cannot do: %v", id, byID[id].Roles)
		}
	}

	// The CLI providers are configured by being installed, so they carry no
	// key material to leak.
	for _, id := range []string{"claude-cli", "codex-cli"} {
		if p := byID[id]; p.KeyHint != "" || p.UserKeyLast4 != "" {
			t.Errorf("%s exposes key fields: %+v", id, p)
		}
	}
}

func TestLocalStatus(t *testing.T) {
	e := newTestEnv(t)
	mustStatus(t, e.do(http.MethodPost, "/api/workflows", map[string]any{"name": "One"}), http.StatusCreated)
	mustStatus(t, e.do(http.MethodPost, "/api/workflows", map[string]any{"name": "Two"}), http.StatusCreated)

	status := decode[struct {
		Project   string          `json:"project"`
		Workflows int             `json:"workflows"`
		Providers map[string]bool `json:"providers"`
		CLI       map[string]bool `json:"cli"`
	}](t, mustStatus(t, e.do(http.MethodGet, "/api/local/status", nil), http.StatusOK))

	if status.Project != e.store.Root {
		t.Errorf("project = %q, want %q", status.Project, e.store.Root)
	}
	if status.Workflows != 2 {
		t.Errorf("workflows = %d, want 2", status.Workflows)
	}
	if _, ok := status.Providers["google"]; !ok {
		t.Errorf("providers = %+v", status.Providers)
	}
	if _, ok := status.CLI["claude"]; !ok {
		t.Errorf("cli = %+v", status.CLI)
	}
	if _, ok := status.CLI["codex"]; !ok {
		t.Errorf("cli = %+v", status.CLI)
	}
}

// CORS stays tight: the app's own origins are echoed, anything else gets no
// header and the browser drops the response.
func TestCORSAllowsOnlyLocalOrigins(t *testing.T) {
	allowed := []string{"http://localhost:5173", "http://127.0.0.1:4102", "https://localhost:8443", "file://", "null"}
	denied := []string{"https://evil.example.com", "http://localhost.evil.com", "http://example.com:5173"}

	for _, o := range allowed {
		if !allowedOrigin(o) {
			t.Errorf("origin %q should be allowed", o)
		}
	}
	for _, o := range denied {
		if allowedOrigin(o) {
			t.Errorf("origin %q should be denied", o)
		}
	}

	e := newTestEnv(t)
	rec := e.raw(http.MethodGet, "/health", nil, map[string]string{"Origin": "http://localhost:5173"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("allow-origin = %q", got)
	}
	rec = e.raw(http.MethodGet, "/health", nil, map[string]string{"Origin": "https://evil.example.com"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q, want empty", got)
	}
	// Preflight has to pass the Authorization header through, or every real
	// request is blocked before it is sent.
	rec = e.raw(http.MethodOptions, "/api/workflows", nil, map[string]string{"Origin": "file://"})
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("allow-headers = %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

// The handshake is the whole startup contract with Electron: one line of JSON
// carrying the port and the token.
func TestHandshakeLine(t *testing.T) {
	e := newTestEnv(t)
	f, err := os.CreateTemp(t.TempDir(), "handshake")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer f.Close()
	if err := e.daemon.writeHandshake(f, 51234); err != nil {
		t.Fatalf("writeHandshake: %v", err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") || strings.Count(string(data), "\n") != 1 {
		t.Fatalf("handshake is not exactly one line: %q", data)
	}
	var hs struct {
		Ready   bool   `json:"ready"`
		Port    int    `json:"port"`
		Token   string `json:"token"`
		Project string `json:"project"`
	}
	if err := json.Unmarshal(data, &hs); err != nil {
		t.Fatalf("decode handshake: %v", err)
	}
	if !hs.Ready || hs.Port != 51234 || hs.Project != e.store.Root {
		t.Errorf("handshake = %+v", hs)
	}
	if len(hs.Token) != 64 {
		t.Errorf("token is %d chars, want 64 hex characters (32 bytes)", len(hs.Token))
	}
	if hs.Token != e.daemon.token {
		t.Errorf("handshake token does not match the one the daemon checks")
	}
}

// sanitizeOutput keeps run files small enough to live in a git-tracked folder.
func TestSanitizeOutputTruncatesDataURLs(t *testing.T) {
	big := `{"type":"image","value":{"dataUrl":"data:image/png;base64,` + strings.Repeat("A", 1000) + `","url":"/content/x.png"}}`
	got := sanitizeOutput(big)
	if strings.Contains(got, strings.Repeat("A", 600)) {
		t.Errorf("data url was not truncated: %d bytes", len(got))
	}
	if !strings.Contains(got, "/content/x.png") {
		t.Errorf("the media URL was dropped: %s", got)
	}
	// A short data URL is left alone, and unparseable input passes through.
	if sanitizeOutput(`not json`) != `not json` {
		t.Error("non-JSON output should pass through unchanged")
	}
}

type executionEnvelope struct {
	Execution localstore.Execution       `json:"execution"`
	Nodes     []localstore.NodeExecution `json:"nodes"`
}

// waitForExecution polls exactly as the builder does, until the run leaves the
// queued/running states.
func (e *testEnv) waitForExecution(id string) executionEnvelope {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var env executionEnvelope
	for time.Now().Before(deadline) {
		rec := e.do(http.MethodGet, "/api/executions/"+id, nil)
		if rec.Code != http.StatusOK {
			e.t.Fatalf("poll status = %d: %s", rec.Code, rec.Body.String())
		}
		env = decode[executionEnvelope](e.t, rec)
		switch env.Execution.Status {
		case "completed", "failed", "cancelled":
			return env
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("execution %s never finished (last status %q)", id, env.Execution.Status)
	return env
}

func mustStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) *httptest.ResponseRecorder {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body.String())
	}
	return rec
}

// The shared web client issues every request with credentials: "include".
// A browser throws away the response to such a request unless the server says
// Allow-Credentials: true, so the dev-mode window went blank with a CORS error
// while the same code worked in the packaged app. This pins the header.
func TestCorsAllowsCredentialedRequests(t *testing.T) {
	env := newTestEnv(t)

	for _, origin := range []string{"http://localhost:5173", "http://127.0.0.1:5173", "file://"} {
		t.Run(origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "/api/workflows", nil)
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", "GET")
			rec := httptest.NewRecorder()
			env.handler.ServeHTTP(rec, req)

			if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
				t.Fatalf("Allow-Credentials = %q, want \"true\"", got)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
				// A wildcard origin is illegal alongside credentials, so the
				// exact origin has to be echoed back.
				t.Fatalf("Allow-Origin = %q, want the request origin %q", got, origin)
			}
		})
	}
}

// An origin that is not this machine gets no CORS headers at all, credentials
// or otherwise. Echoing one would let any page the user visits drive the daemon.
func TestCorsRefusesAForeignOrigin(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodOptions, "/api/workflows", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Allow-Origin = %q, want it absent", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Allow-Credentials = %q, want it absent", got)
	}
}

// fileGraph reads a file from the project folder and writes it back somewhere
// else. Like providerFreeGraph it needs no model, so it exercises the one thing
// this test is about: the daemon handing the engine the folder it opened.
const fileGraph = `{
  "nodes": [
    {"id":"n1","type":"fileInput","position":{"x":0,"y":0},"data":{"config":{"path":"notes/input.txt"}}},
    {"id":"n2","type":"fileOutput","position":{"x":300,"y":0},"data":{"config":{"path":"out/copy.txt"}}},
    {"id":"n3","type":"output","position":{"x":600,"y":0},"data":{"config":{}}}
  ],
  "edges": [
    {"id":"e1","source":"n1","target":"n2","sourceHandle":"out","targetHandle":"in","type":"data"},
    {"id":"e2","source":"n2","target":"n3","sourceHandle":"out","targetHandle":"in","type":"data"}
  ]
}`

// The file nodes are the reason the desktop app exists, so a run has to reach
// the user's own files — not .zyvro, and not a copy of them.
func TestExecutionReadsAndWritesTheProjectFolder(t *testing.T) {
	e := newTestEnv(t)
	if err := os.MkdirAll(filepath.Join(e.store.Root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.store.Root, "notes", "input.txt"), []byte("what the user wrote"), 0o644); err != nil {
		t.Fatal(err)
	}

	wf := decode[localstore.Workflow](t, mustStatus(t,
		e.do(http.MethodPost, "/api/workflows", map[string]any{
			"name":       "Files",
			"graph_json": json.RawMessage(fileGraph),
		}), http.StatusCreated))
	start := decode[struct {
		ExecutionID string `json:"execution_id"`
	}](t, mustStatus(t, e.do(http.MethodPost, "/api/executions", map[string]any{
		"workflow_id": wf.ID,
	}), http.StatusAccepted))

	final := e.waitForExecution(start.ExecutionID)
	if final.Execution.Status != "completed" {
		t.Fatalf("status = %q, error = %q", final.Execution.Status, final.Execution.Error)
	}
	written, err := os.ReadFile(filepath.Join(e.store.Root, "out", "copy.txt"))
	if err != nil {
		t.Fatalf("the workflow did not write the file: %v", err)
	}
	if string(written) != "what the user wrote" {
		t.Fatalf("written = %q", written)
	}
	// The path it saved to travels on with the value, so the run's result says
	// where the file went.
	if !strings.Contains(final.Execution.OutputJSON, "out/copy.txt") {
		t.Errorf("output_json = %q", final.Execution.OutputJSON)
	}
}

// The builder is shared with the hosted app and asks the backend what it can
// run, so the daemon has to say that the file nodes work here.
func TestLocalStatusAdvertisesTheFileNodes(t *testing.T) {
	e := newTestEnv(t)
	status := decode[struct {
		LocalNodes []string `json:"local_nodes"`
	}](t, mustStatus(t, e.do(http.MethodGet, "/api/local/status", nil), http.StatusOK))

	want := map[string]bool{"fileInput": true, "fileOutput": true}
	for _, n := range status.LocalNodes {
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("local_nodes = %v, missing %v", status.LocalNodes, want)
	}
}

// The order this project wants its providers tried in.
//
// The daemon is the engine, so what it accepts here decides where a run goes.
// A list naming a provider for a job it cannot do — Black Forest Labs for
// vision — would send every such call to a backend that cannot answer, and
// nothing in the failure would say the ordering was the reason.
func TestProviderOrderIsCleanedAndKept(t *testing.T) {
	e := newTestEnv(t)

	read := func() map[string][]string {
		listing := decode[struct {
			Order map[string][]string `json:"order"`
		}](t, mustStatus(t, e.do(http.MethodGet, "/api/providers", nil), http.StatusOK))
		return listing.Order
	}

	if got := read(); len(got) != 0 {
		t.Fatalf("a fresh project already had an order: %v", got)
	}

	saved := decode[struct {
		Order map[string][]string `json:"order"`
	}](t, mustStatus(t, e.do(http.MethodPut, "/api/providers/order", map[string]any{
		"order": map[string][]string{
			"vision":   {"ollama", "google"},
			"text":     {"claude-cli", "ollama"},
			"image":    {"ollama", "google"}, // ollama cannot generate
			"nonsense": {"google"},           // not a job
		},
	}), http.StatusOK))

	if strings.Join(saved.Order["vision"], ",") != "ollama,google" {
		t.Errorf("a legitimate vision order was altered: %v", saved.Order["vision"])
	}
	if strings.Join(saved.Order["image"], ",") != "google" {
		t.Errorf("Ollama was kept for image generation: %v", saved.Order["image"])
	}
	if _, ok := saved.Order["nonsense"]; ok {
		t.Error("a job that does not exist was kept")
	}

	// And it survives, because it is written to the project rather than held in
	// memory: a daemon restarts every time the app opens the folder again.
	if got := read(); strings.Join(got["vision"], ",") != "ollama,google" {
		t.Fatalf("the order did not come back: %v", got)
	}
}
