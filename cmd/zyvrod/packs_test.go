package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/engine"
	"github.com/Zyvro/Zyvro-engine/localstore"
)

// No test here makes a real model call. The pack nodes these tests run are the
// ones that need no model at all; the one that does is never executed.

// ---------- harness ----------

// packEnv is newTestEnv with packs already in the project, because packs are
// read when the project opens and a pack installed afterwards would not be the
// case under test.
type packEnv struct {
	*testEnv
	logs *bytes.Buffer
}

func newPackEnv(t *testing.T, setup func(packsDir string)) *packEnv {
	t.Helper()
	root := t.TempDir()
	packsDir := filepath.Join(root, ".zyvro", "packs")
	if err := os.MkdirAll(packsDir, 0o755); err != nil {
		t.Fatalf("create packs dir: %v", err)
	}
	if setup != nil {
		setup(packsDir)
	}

	// The daemon's account of what loaded and what was refused goes to the
	// standard logger, which is where it would go on a user's machine, so the
	// tests read it there rather than through a seam invented for them.
	logs := &bytes.Buffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	store, err := localstore.Open(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	d, err := newDaemon(store)
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	return &packEnv{
		testEnv: &testEnv{t: t, daemon: d, handler: d.handler(), store: store},
		logs:    logs,
	}
}

// installFixturePack copies a pack out of testdata into the project. The packs
// are copies rather than references to a folder on the machine that wrote them:
// a test depending on /tmp is a test that passes exactly once.
func installFixturePack(t *testing.T, packsDir, name string) {
	t.Helper()
	if err := os.CopyFS(filepath.Join(packsDir, name), os.DirFS(filepath.Join("testdata", "packs", name))); err != nil {
		t.Fatalf("install fixture pack %s: %v", name, err)
	}
}

// writeBrokenPack puts a pack in the folder that cannot possibly load. The
// manifest is the broken part on purpose: it is the failure that leaves the
// loader with nothing but the folder name to call the pack by.
func writeBrokenPack(t *testing.T, packsDir, name string) {
	t.Helper()
	dir := filepath.Join(packsDir, name)
	if err := os.MkdirAll(filepath.Join(dir, "nodes"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zyvro-pack.json"), []byte(`{ this is not JSON`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func (e *packEnv) nodeCatalogue() []map[string]json.RawMessage {
	e.t.Helper()
	rec := mustStatus(e.t, e.do(http.MethodGet, "/api/nodes", nil), http.StatusOK)
	return decode[[]map[string]json.RawMessage](e.t, rec)
}

func findKind(kinds []map[string]json.RawMessage, nodeType string) map[string]json.RawMessage {
	for _, k := range kinds {
		var got string
		if json.Unmarshal(k["type"], &got) == nil && got == nodeType {
			return k
		}
	}
	return nil
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------- GET /api/nodes ----------

// TestNodeCatalogueServesBuiltInsAndPackNodes: the desktop no longer carries
// the built-in set with it, so this one response is the whole palette.
func TestNodeCatalogueServesBuiltInsAndPackNodes(t *testing.T) {
	e := newPackEnv(t, func(dir string) { installFixturePack(t, dir, "text-tools") })
	kinds := e.nodeCatalogue()

	// The whole palette: what the engine still implements in Go, what its
	// bundled pack contributes, and the two nodes this project's pack added.
	if got, want := len(kinds), len(engine.Catalogue(nil))+2; got != want {
		t.Fatalf("catalogue has %d entries, want %d (everything the engine ships plus the pack's two nodes)", got, want)
	}

	builtin := findKind(kinds, "llm")
	if builtin == nil {
		t.Fatal("the catalogue does not include the llm node")
	}
	if _, shadowed := builtin["pack"]; shadowed {
		t.Error("a built-in was served with a pack name")
	}

	plugin := findKind(kinds, "wordCount")
	if plugin == nil {
		t.Fatalf("the catalogue does not include the pack's wordCount node: %v", kinds)
	}
	var pack string
	if err := json.Unmarshal(plugin["pack"], &pack); err != nil || pack != "text-tools" {
		t.Errorf("wordCount pack = %q (%v), want text-tools", pack, err)
	}
	// The badge is the point of the field: a node that behaves oddly should be
	// identifiable as not ours at a glance, which needs a name, not a boolean.
	var category, label string
	_ = json.Unmarshal(plugin["category"], &category)
	_ = json.Unmarshal(plugin["label"], &label)
	if category != "Utility" || label != "Word Count" {
		t.Errorf("wordCount label/category = %q/%q", label, category)
	}
}

// TestNodeCatalogueMatchesTheFrontendShape: the response is handed straight to
// the builder's NodeKind type, so an extra field is as wrong as a missing one.
func TestNodeCatalogueMatchesTheFrontendShape(t *testing.T) {
	e := newPackEnv(t, func(dir string) { installFixturePack(t, dir, "text-tools") })
	kinds := e.nodeCatalogue()

	base := []string{"type", "label", "category", "description", "inputs", "outputs", "defaults"}
	for _, tc := range []struct {
		nodeType string
		want     []string
	}{
		{"llm", base},
		{"fileInput", append(append([]string{}, base...), "localOnly")},
		{"zyvroTools", append(append([]string{}, base...), "toolOnly")},
		{"wordCount", append(append([]string{}, base...), "pack")},
	} {
		kind := findKind(kinds, tc.nodeType)
		if kind == nil {
			t.Errorf("%s is missing from the catalogue", tc.nodeType)
			continue
		}
		missing, extra := diffStrings(keysOf(kind), tc.want)
		if len(missing) > 0 || len(extra) > 0 {
			t.Errorf("%s: missing fields %v, unexpected fields %v", tc.nodeType, missing, extra)
		}
	}

	// inputs, outputs and defaults are always present, never null: the builder
	// reads .length off the first two and spreads the third.
	for _, kind := range kinds {
		for _, field := range []string{"inputs", "outputs", "defaults"} {
			if string(kind[field]) == "null" {
				t.Errorf("%s: %s is null", kind["type"], field)
			}
		}
	}
}

func diffStrings(got, want []string) (missing, extra []string) {
	inGot := map[string]bool{}
	for _, s := range got {
		inGot[s] = true
	}
	inWant := map[string]bool{}
	for _, s := range want {
		inWant[s] = true
	}
	for _, s := range want {
		if !inGot[s] {
			missing = append(missing, s)
		}
	}
	for _, s := range got {
		if !inWant[s] {
			extra = append(extra, s)
		}
	}
	return missing, extra
}

// ---------- GET /api/packs ----------

type packResponse struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	NodeTypes    []string `json:"node_types"`
	Dir          string   `json:"dir"`
	Error        string   `json:"error"`
}

func (e *packEnv) packs() []packResponse {
	e.t.Helper()
	rec := mustStatus(e.t, e.do(http.MethodGet, "/api/packs", nil), http.StatusOK)
	return decode[[]packResponse](e.t, rec)
}

// TestPacksReportsABrokenPackWithItsError: a pack that vanishes from the list
// when it breaks is a pack the user cannot debug, so it stays, carrying the
// loader's own words.
func TestPacksReportsABrokenPackWithItsError(t *testing.T) {
	e := newPackEnv(t, func(dir string) {
		installFixturePack(t, dir, "text-tools")
		writeBrokenPack(t, dir, "half-finished")
	})

	packs := e.packs()
	if len(packs) != 2 {
		t.Fatalf("packs = %+v, want both the good one and the broken one", packs)
	}

	byName := map[string]packResponse{}
	for _, p := range packs {
		byName[p.Name] = p
	}

	good, ok := byName["text-tools"]
	if !ok {
		t.Fatal("the pack that loaded is not listed")
	}
	if good.Error != "" {
		t.Errorf("text-tools reported an error: %s", good.Error)
	}
	if good.Version != "0.1.0" || len(good.NodeTypes) != 2 {
		t.Errorf("text-tools = %+v", good)
	}
	if len(good.Capabilities) != 1 || good.Capabilities[0] != "llm" {
		t.Errorf("text-tools capabilities = %v, want [llm]", good.Capabilities)
	}

	// The broken one is named by its folder, which is the only name it has left
	// once the manifest is the part that will not parse.
	bad, ok := byName["half-finished"]
	if !ok {
		t.Fatalf("the broken pack was omitted: %+v", packs)
	}
	if bad.Error == "" {
		t.Fatal("the broken pack was listed without a reason")
	}
	if !strings.Contains(bad.Error, "zyvro-pack.json") {
		t.Errorf("error does not say which file is wrong: %q", bad.Error)
	}
	if !strings.Contains(bad.Dir, "half-finished") {
		t.Errorf("dir = %q, want the folder it was loaded from", bad.Dir)
	}

	// The same account reaches the log, which is where someone looks when the
	// app will not show them a panel.
	logs := e.logs.String()
	if !strings.Contains(logs, "half-finished was refused") {
		t.Errorf("the refusal was not logged: %s", logs)
	}
	if !strings.Contains(logs, "loaded text-tools 0.1.0") {
		t.Errorf("the successful load was not logged: %s", logs)
	}
}

// TestAMalformedPackDoesNotPreventTheProjectFromOpening is the rule the whole
// loading path is shaped around. Someone who pulls a branch with a half-written
// pack on it must end up looking at their workflows and an error message, not
// at an app that will not start.
func TestAMalformedPackDoesNotPreventTheProjectFromOpening(t *testing.T) {
	e := newPackEnv(t, func(dir string) {
		writeBrokenPack(t, dir, "totally-broken")
		// A directory that is not a pack at all, and a stray file next to the
		// packs: neither is an error, and neither may stop the rest loading.
		if err := os.MkdirAll(filepath.Join(dir, "empty-folder"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a pack"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		installFixturePack(t, dir, "text-tools")
	})

	// The project opened: the ordinary routes work.
	mustStatus(t, e.do(http.MethodGet, "/api/workflows", nil), http.StatusOK)

	// And the pack that was fine is installed despite the ones that were not.
	if findKind(e.nodeCatalogue(), "wordCount") == nil {
		t.Error("a broken pack prevented a working one from loading")
	}
	if len(e.packs()) != 3 {
		t.Errorf("packs = %+v, want the three directories (the stray file is not a pack)", e.packs())
	}
}

// ---------- status ----------

type statusPacks struct {
	Packs       int      `json:"packs"`
	PackNames   []string `json:"pack_names"`
	PacksFailed int      `json:"packs_failed"`
}

func TestLocalStatusCountsAndNamesPacks(t *testing.T) {
	e := newPackEnv(t, func(dir string) {
		installFixturePack(t, dir, "text-tools")
		installFixturePack(t, dir, "probe")
		writeBrokenPack(t, dir, "half-finished")
	})

	status := decode[statusPacks](t, mustStatus(t, e.do(http.MethodGet, "/api/local/status", nil), http.StatusOK))

	// The count is of packs that loaded. A bar reading "3 packs" when one of
	// them contributes nothing would be worse than no bar at all.
	if status.Packs != 2 {
		t.Errorf("packs = %d, want the 2 that loaded", status.Packs)
	}
	if len(status.PackNames) != 2 || status.PackNames[0] != "probe" || status.PackNames[1] != "text-tools" {
		t.Errorf("pack_names = %v, want the loaded ones, sorted", status.PackNames)
	}
	// The one that did not is still counted, or a node missing from the palette
	// has no explanation anywhere the user is looking.
	if status.PacksFailed != 1 {
		t.Errorf("packs_failed = %d, want 1", status.PacksFailed)
	}
}

// A project with no packs still answers, with zero and an empty list rather
// than a missing field the status bar would have to guess at.
func TestLocalStatusWithNoPacks(t *testing.T) {
	e := newTestEnv(t)
	status := decode[statusPacks](t, mustStatus(t, e.do(http.MethodGet, "/api/local/status", nil), http.StatusOK))
	if status.Packs != 0 || len(status.PackNames) != 0 || status.PacksFailed != 0 {
		t.Errorf("packs = %d, names = %v, failed = %d", status.Packs, status.PackNames, status.PacksFailed)
	}
	if rec := e.do(http.MethodGet, "/api/packs", nil); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("packs body = %q, want an empty list", rec.Body.String())
	}
}

// ---------- the token ----------

func TestNodeAndPackRoutesNeedTheToken(t *testing.T) {
	e := newTestEnv(t)
	for _, target := range []string{"/api/nodes", "/api/packs"} {
		rec := e.raw(http.MethodGet, target, nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: status = %d, want 401", target, rec.Code)
		}
	}
	rec := e.raw(http.MethodPost, "/api/packs/reload", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("reload without a token: status = %d, want 401", rec.Code)
	}
}

// ---------- reloading ----------

// TestReloadPicksUpAPackInstalledAfterOpening: editing a pack is an edit loop,
// and closing the project between iterations is not one.
func TestReloadPicksUpAPackInstalledAfterOpening(t *testing.T) {
	e := newPackEnv(t, nil)
	if findKind(e.nodeCatalogue(), "wordCount") != nil {
		t.Fatal("a pack was loaded from an empty folder")
	}

	installFixturePack(t, e.store.PacksDir(), "text-tools")
	mustStatus(t, e.do(http.MethodPost, "/api/packs/reload", nil), http.StatusOK)

	if findKind(e.nodeCatalogue(), "wordCount") == nil {
		t.Error("the reload did not pick up the new pack")
	}

	// And a reload after the pack is removed drops it, rather than merging the
	// new folder into the old registry.
	if err := os.RemoveAll(filepath.Join(e.store.PacksDir(), "text-tools")); err != nil {
		t.Fatalf("remove pack: %v", err)
	}
	mustStatus(t, e.do(http.MethodPost, "/api/packs/reload", nil), http.StatusOK)
	if findKind(e.nodeCatalogue(), "wordCount") != nil {
		t.Error("a removed pack is still installed")
	}
}

// ---------- running one ----------

// packGraph is a text input feeding one pack node and a preview.
func packGraph(nodeType string) json.RawMessage {
	return json.RawMessage(`{
	  "nodes": [
	    {"id":"n1","type":"textInput","position":{"x":0,"y":0},"data":{"config":{"value":"one two three four"}}},
	    {"id":"n2","type":"` + nodeType + `","position":{"x":300,"y":0},"data":{"config":{}}},
	    {"id":"n3","type":"preview","position":{"x":600,"y":0},"data":{"config":{}}}
	  ],
	  "edges": [
	    {"id":"e1","source":"n1","target":"n2","sourceHandle":"out","targetHandle":"in","type":"data"},
	    {"id":"e2","source":"n2","target":"n3","sourceHandle":"out","targetHandle":"in","type":"data"}
	  ]
	}`)
}

func (e *packEnv) runGraph(graph json.RawMessage) executionEnvelope {
	e.t.Helper()
	wf := decode[localstore.Workflow](e.t, mustStatus(e.t,
		e.do(http.MethodPost, "/api/workflows", map[string]any{
			"name":       "Pack run",
			"graph_json": graph,
		}), http.StatusCreated))
	start := decode[struct {
		ExecutionID string `json:"execution_id"`
	}](e.t, mustStatus(e.t, e.do(http.MethodPost, "/api/executions", map[string]any{
		"workflow_id": wf.ID,
	}), http.StatusAccepted))
	return e.waitForExecution(start.ExecutionID)
}

// TestARunUsesThePacksTheProjectCarries: the registry has to reach the runtime,
// not just the catalogue. wordCount needs no model, so this run touches nothing
// off the machine.
func TestARunUsesThePacksTheProjectCarries(t *testing.T) {
	e := newPackEnv(t, func(dir string) { installFixturePack(t, dir, "text-tools") })

	final := e.runGraph(packGraph("wordCount"))
	if final.Execution.Status != "completed" {
		t.Fatalf("status = %q, error = %q", final.Execution.Status, final.Execution.Error)
	}
	if !strings.Contains(final.Execution.OutputJSON, `"words":4`) {
		t.Errorf("output_json = %q, want the pack node's count", final.Execution.OutputJSON)
	}
}

// TestAPackNodesLogLandsOnItsRecord: a pack node is the user's own code, and
// when it fails what it printed is the only account of what it was doing. The
// run stops at the failure, so nothing later would flush it.
func TestAPackNodesLogLandsOnItsRecord(t *testing.T) {
	e := newPackEnv(t, func(dir string) { installFixturePack(t, dir, "probe") })

	final := e.runGraph(packGraph("noisyFailure"))
	if final.Execution.Status != "failed" {
		t.Fatalf("status = %q, want failed", final.Execution.Status)
	}
	var node *localstore.NodeExecution
	for i := range final.Nodes {
		if final.Nodes[i].NodeID == "n2" {
			node = &final.Nodes[i]
		}
	}
	if node == nil {
		t.Fatalf("no record for the failing node: %+v", final.Nodes)
	}
	if node.Status != "failed" || node.Error == "" {
		t.Errorf("node = %+v, want a failure with a reason", node)
	}
	if got := strings.Join(node.Log, "\n"); !strings.Contains(got, "first line") || !strings.Contains(got, "second line") {
		t.Errorf("log = %q, want what the node printed before it gave up", got)
	}
}
