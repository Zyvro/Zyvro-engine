package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/plugins"
	"github.com/Zyvro/Zyvro-engine/providers"
)

// No test in this file makes a real model call. The two that need one point a
// providers.Config at an httptest server on loopback, which is the same trick
// the sandbox's own tests use one layer down: the only LLM a node ever sees is
// the one the host hands it, so faking the host's is enough.

// ---------- fixtures ----------

// fixtureRegistry loads packs from engine/testdata/packs. They are copies, not
// references to a folder somewhere on the machine that wrote them: a test that
// depends on /tmp is a test that passes exactly once.
func fixtureRegistry(t *testing.T, names ...string) *plugins.Registry {
	t.Helper()
	reg := plugins.NewRegistry()
	for _, name := range names {
		pack, err := plugins.Load(filepath.Join("testdata", "packs", name))
		if err != nil {
			t.Fatalf("load pack %s: %v", name, err)
		}
		if err := reg.Install(pack); err != nil {
			t.Fatalf("install pack %s: %v", name, err)
		}
	}
	return reg
}

// thenPreview builds a graph around a node that takes no input. The preview is
// not decoration: the DAG loop skips a node no data edge touches, on the
// grounds that it is a tool node bound to a Brain, so an input-less node needs
// something downstream of it to be executed at all.
func thenPreview(nodeType string, cfg map[string]any) *Graph {
	if cfg == nil {
		cfg = map[string]any{}
	}
	return &Graph{
		Nodes: []GraphNode{
			{ID: "B", Type: nodeType, Data: map[string]any{"config": cfg}},
			{ID: "C", Type: "preview", Data: map[string]any{"config": map[string]any{}}},
		},
		Edges: []GraphEdge{
			{ID: "e1", Source: "B", Target: "C", SourceHandle: "out", TargetHandle: "in", Type: "data"},
		},
	}
}

// textThen builds the smallest useful graph: a text input feeding one node.
func textThen(text, nodeType string, cfg map[string]any) *Graph {
	if cfg == nil {
		cfg = map[string]any{}
	}
	return &Graph{
		Nodes: []GraphNode{
			{ID: "A", Type: "textInput", Data: map[string]any{"config": map[string]any{"value": text}}},
			{ID: "B", Type: nodeType, Data: map[string]any{"config": cfg}},
		},
		Edges: []GraphEdge{
			{ID: "e1", Source: "A", Target: "B", SourceHandle: "out", TargetHandle: "in", Type: "data"},
		},
	}
}

// fakeOpenAI is a chat-completions endpoint that answers with a fixed string
// and remembers what it was asked. Pointing OpenAIBaseURL at it is what makes
// "the node called a model" observable without any model existing.
type fakeOpenAI struct {
	*httptest.Server
	requests []map[string]any
}

func newFakeOpenAI(t *testing.T, reply string) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.requests = append(f.requests, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply}}},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeOpenAI) config() *providers.Config {
	return &providers.Config{
		TextProvider:  "openai",
		OpenAIBaseURL: f.URL,
		OpenAIAPIKey:  "test-key-not-a-real-one",
		OpenAIModel:   "fake-model",
	}
}

// countingReporter is the minimum NodeReporter, plus the optional NodeLogger,
// so a test can see both halves of what the engine reports.
type countingReporter struct {
	logs   map[string][]string
	failed map[string]string
}

func newCountingReporter() *countingReporter {
	return &countingReporter{logs: map[string][]string{}, failed: map[string]string{}}
}

func (c *countingReporter) NodeStart(nodeID, nodeType string) func(*NodeOutput, error) {
	return func(_ *NodeOutput, err error) {
		if err != nil {
			c.failed[nodeID] = err.Error()
		}
	}
}

func (c *countingReporter) NodeLog(nodeID string, lines []string) {
	c.logs[nodeID] = append(c.logs[nodeID], lines...)
}

// ---------- running a pack node ----------

// TestPluginNodeRunsLikeABuiltIn: the whole point of the wiring. A node type
// that exists only as Lua in a project's pack folder has to execute from the
// DAG loop and leave an ordinary NodeOutput behind it.
func TestPluginNodeRunsLikeABuiltIn(t *testing.T) {
	rt := NewRuntime("exec1", textThen("one two three", "wordCount", nil), nil, nil, nil)
	rt.Plugins = fixtureRegistry(t, "text-tools")

	if err := rt.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := rt.Outputs["B"]
	if out == nil || out.Type != "json" {
		t.Fatalf("output = %+v, want a json output", out)
	}
	data, ok := out.Value["data"].(map[string]any)
	if !ok {
		t.Fatalf("value = %+v, want data", out.Value)
	}
	if got := num(data["words"], 0); got != 3 {
		t.Errorf("words = %v, want 3", data["words"])
	}
	if got := num(data["characters"], 0); got != 13 {
		t.Errorf("characters = %v, want 13", data["characters"])
	}
}

// TestPluginNodeReachesTheModelThroughTheHost: a pack names a model at most,
// and the provider, the credential and the endpoint stay on the Go side.
func TestPluginNodeReachesTheModelThroughTheHost(t *testing.T) {
	fake := newFakeOpenAI(t, "a short summary")
	graph := textThen("a long piece of text", "summarize", map[string]any{
		"sentences": 2,
		"tone":      "technical",
		"provider":  "openai",
	})
	rt := NewRuntime("exec1", graph, fake.config(), nil, nil)
	rt.Plugins = fixtureRegistry(t, "text-tools")

	if err := rt.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := str(rt.Outputs["B"].Value["text"]); got != "a short summary" {
		t.Fatalf("text = %q", got)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("model was called %d times, want once", len(fake.requests))
	}
	// The node's own config reached the prompt, which is what says the config
	// crossed the boundary rather than the defaults being used.
	body, _ := json.Marshal(fake.requests[0])
	if !strings.Contains(string(body), "technical") {
		t.Errorf("request did not carry the node's tone: %s", body)
	}
}

// TestPluginConfigPlaceholdersAreResolvedBeforeTheScriptSeesThem: resolving
// {{input:...}} is the runtime's job. A pack that had to do it itself would be
// a pack that could decide not to.
func TestPluginConfigPlaceholdersAreResolvedBeforeTheScriptSeesThem(t *testing.T) {
	files := newFakeFiles()
	files.read["notes/today.txt"] = []byte("the contents")
	graph := textThen("", "readText", map[string]any{"path": "notes/{{input:name}}"})
	rt := NewRuntime("exec1", graph, nil, nil, map[string]any{"name": "today.txt"})
	rt.Plugins = fixtureRegistry(t, "file-tools")
	rt.Files = files

	if err := rt.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := str(rt.Outputs["B"].Value["text"]); got != "the contents" {
		t.Fatalf("text = %q", got)
	}
	if len(files.reads) != 1 || files.reads[0] != "notes/today.txt" {
		t.Fatalf("the script asked for %v, want the resolved path", files.reads)
	}
}

// ---------- the unknown type ----------

// TestUnknownNodeTypeNamesItselfAndSaysAPackMayBeMissing: the error is read by
// someone who has just opened a workflow a colleague sent them, and "unknown
// node type" on its own does not tell them what to do about it.
func TestUnknownNodeTypeNamesItselfAndSaysAPackMayBeMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  *plugins.Registry
	}{
		{"no packs installed at all", nil},
		{"packs installed, but not this one", fixtureRegistry(t, "text-tools")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := NewRuntime("exec1", textThen("hi", "notARealNode", nil), nil, nil, nil)
			rt.Plugins = tc.reg

			err := rt.Execute(context.Background())
			if err == nil {
				t.Fatal("an unknown node type ran")
			}
			if !strings.Contains(err.Error(), "notARealNode") {
				t.Errorf("error does not name the type: %v", err)
			}
			if !strings.Contains(err.Error(), "pack") {
				t.Errorf("error does not mention a pack: %v", err)
			}
		})
	}
}

// ---------- the replay cache ----------

// TestPluginNodeIsNeverReplayedFromTheCache: a fingerprint hashes the graph,
// and not one byte of a pack's Lua is in the graph. Replaying a plugin node
// would mean editing its script and getting last week's answer forever.
func TestPluginNodeIsNeverReplayedFromTheCache(t *testing.T) {
	graph := textThen("one two three", "wordCount", nil)
	rt := NewRuntime("exec2", graph, nil, nil, nil)
	rt.Plugins = fixtureRegistry(t, "text-tools")

	fps, _, err := ComputeFingerprints(graph, nil)
	if err != nil {
		t.Fatalf("fingerprints: %v", err)
	}
	rt.Fingerprints = fps
	// Both nodes are offered a cache entry under their current fingerprint, so
	// the only difference between them is what they are.
	rt.Cache = map[string]CacheEntry{
		"A": {NodeID: "A", Fingerprint: fps["A"], Output: textOutput("cached input")},
		"B": {NodeID: "B", Fingerprint: fps["B"], Output: textOutput("cached, and wrong")},
	}

	if err := rt.ExecuteWithReporting(context.Background(), newCountingReporter()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The built-in was replayed: that is the control, and without it this test
	// would also pass against a cache that was never consulted.
	if !rt.Replayed["A"] {
		t.Error("the built-in node was not replayed, so the cache was not wired up")
	}
	if rt.Replayed["B"] {
		t.Fatal("the plugin node was served from the replay cache")
	}
	if rt.Outputs["B"].Type != "json" {
		t.Fatalf("plugin output = %+v, want the freshly computed one", rt.Outputs["B"])
	}
}

// ---------- capabilities ----------

// TestFilesCapabilityDecidesWhetherFileAccessIsHandedOver is the engine-side
// half of the files capability. The sandbox checks it again before exposing
// ctx.readFile, but a pack that did not declare it must not be handed a live
// FileAccess at all, so that a bug in the far side of the boundary has nothing
// to leak.
func TestFilesCapabilityDecidesWhetherFileAccessIsHandedOver(t *testing.T) {
	reg := fixtureRegistry(t, "text-tools", "file-tools")
	rt := NewRuntime("exec1", &Graph{}, nil, nil, nil)
	rt.Plugins = reg
	rt.Files = newFakeFiles()

	for _, tc := range []struct {
		nodeType string
		want     bool
	}{
		{"wordCount", false}, // its pack declares llm, and only llm
		{"readText", true},   // its pack declares files
	} {
		def, ok := reg.Kind(tc.nodeType)
		if !ok {
			t.Fatalf("%s is not registered", tc.nodeType)
		}
		host := rt.pluginHostInput(def, &RunInput{
			Node:   &GraphNode{ID: "n", Type: tc.nodeType},
			Config: map[string]any{},
		})
		if got := host.Files != nil; got != tc.want {
			t.Errorf("%s: got a FileAccess = %v, want %v", tc.nodeType, got, tc.want)
		}
	}
}

// TestAPackWithNoCapabilitiesGetsNoHostFunctions is the same guarantee seen
// from inside the sandbox, where it is what a node author actually observes.
func TestAPackWithNoCapabilitiesGetsNoHostFunctions(t *testing.T) {
	fake := newFakeOpenAI(t, "should never be reached")
	graph := thenPreview("reportCapabilities", nil)
	rt := NewRuntime("exec1", graph, fake.config(), nil, nil)
	rt.Plugins = fixtureRegistry(t, "probe")
	rt.Files = newFakeFiles()

	if err := rt.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	data := rt.Outputs["B"].Value["data"].(map[string]any)
	for _, fn := range []string{"llm", "readFile", "writeFile"} {
		if data[fn] != false {
			t.Errorf("ctx.%s was present for a pack that declared no capabilities", fn)
		}
	}
	if len(fake.requests) != 0 {
		t.Errorf("the model was called %d times by a node that cannot call it", len(fake.requests))
	}
}

// ---------- the log ----------

// TestPluginLogSurvivesAFailingNode: the log is the reason the reporting was
// extended at all. A node that printed three lines and then gave up is exactly
// the node whose author needs those three lines, and the run stops there, so
// nothing later would ever flush them.
func TestPluginLogSurvivesAFailingNode(t *testing.T) {
	graph := thenPreview("noisyFailure", nil)
	rt := NewRuntime("exec1", graph, nil, nil, nil)
	rt.Plugins = fixtureRegistry(t, "probe")

	rep := newCountingReporter()
	err := rt.ExecuteWithReporting(context.Background(), rep)
	if err == nil {
		t.Fatal("the node was supposed to fail")
	}
	if _, ok := rep.failed["B"]; !ok {
		t.Error("the failure did not reach the reporter")
	}
	if got := strings.Join(rep.logs["B"], "\n"); !strings.Contains(got, "first line") || !strings.Contains(got, "second line") {
		t.Errorf("log = %q, want both printed lines", got)
	}
	// The runtime keeps its own copy too, for a caller using Execute, which has
	// no reporter to push to.
	if len(rt.PluginLogs["B"]) != 2 {
		t.Errorf("PluginLogs = %v, want two lines", rt.PluginLogs["B"])
	}
}
