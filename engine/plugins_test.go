package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
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
	// The engine's own constructor, so the fixtures sit next to the bundled
	// pack exactly as an installed pack would.
	reg := NewRegistry()
	for _, name := range names {
		pack, err := plugins.Load(filepath.Join("testdata", "packs", name), reg.Reserved())
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

// A pack node may be replayed, and its code is what decides. Before the code
// digest existed the only safe answer was never, which meant the cache had a
// hole exactly where the expensive nodes were about to move.
func TestAPluginNodeIsReplayedWhileItsCodeIsUnchanged(t *testing.T) {
	graph := textThen("one two three", "wordCount", nil)
	rt := NewRuntime("exec2", graph, nil, nil, nil)
	rt.Plugins = fixtureRegistry(t, "text-tools")

	fps, _, err := rt.ComputeFingerprints(nil)
	if err != nil {
		t.Fatalf("fingerprints: %v", err)
	}
	rt.Fingerprints = fps
	rt.Cache = map[string]CacheEntry{
		"A": {NodeID: "A", Fingerprint: fps["A"], Output: textOutput("cached input")},
		"B": {NodeID: "B", Fingerprint: fps["B"], Output: textOutput("cached plugin answer")},
	}

	if err := rt.ExecuteWithReporting(context.Background(), newCountingReporter()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The built-in is the control: without it this would also pass against a
	// cache that was never consulted at all.
	if !rt.Replayed["A"] {
		t.Error("the built-in node was not replayed, so the cache was not wired up")
	}
	if !rt.Replayed["B"] {
		t.Fatal("a plugin node whose code has not changed was not replayed")
	}
}

// The point of hashing the code: edit the script, and the answer the old
// script gave stops being offered. Simulated by fingerprinting against a
// different digest for the same type, which is what a re-read of an edited
// file produces.
func TestAPluginNodeIsNotReplayedOnceItsCodeChanges(t *testing.T) {
	graph := textThen("one two three", "wordCount", nil)
	rt := NewRuntime("exec2", graph, nil, nil, nil)
	rt.Plugins = fixtureRegistry(t, "text-tools")

	before, _, err := rt.ComputeFingerprints(nil)
	if err != nil {
		t.Fatalf("fingerprints: %v", err)
	}
	edited, _, err := ComputeFingerprints(graph, nil, func(nodeType string) (string, bool) {
		if nodeType == "wordCount" {
			return "the-script-after-an-edit", true
		}
		return rt.nodeCode(nodeType)
	})
	if err != nil {
		t.Fatalf("fingerprints: %v", err)
	}

	if before["A"] != edited["A"] {
		t.Error("editing one node's script changed an unrelated node's fingerprint")
	}
	if before["B"] == edited["B"] {
		t.Fatal("editing a node's script left its fingerprint unchanged, so the old answer would replay forever")
	}

	// And the cache recorded under the old fingerprint is not offered.
	rt.Fingerprints = edited
	rt.Cache = map[string]CacheEntry{
		"B": {NodeID: "B", Fingerprint: before["B"], Output: textOutput("the answer the old script gave")},
	}
	if _, ok := rt.cachedOutput("B"); ok {
		t.Fatal("a cache entry from before the edit was offered for replay")
	}
}

// A node that reaches the project folder stays outside the fingerprint however
// stable its code is: the fingerprint describes the graph, and that node's
// result depends on a disk the graph says nothing about.
func TestAPluginNodeThatReachesTheProjectFolderIsNeverReplayed(t *testing.T) {
	rt := NewRuntime("exec3", &Graph{}, nil, nil, nil)
	rt.Plugins = fixtureRegistry(t, "text-tools", "file-tools")

	for nodeType, want := range map[string]bool{
		"readText":  true,  // le pack file-tools déclare la capacité files
		"wordCount": false, // texte pur, entièrement décrit par le graphe
		"llm":       false, // pack embarqué
		"fileInput": true,  // nœud Go qui lit le disque
	} {
		if got := rt.outsideTheFingerprint(nodeType); got != want {
			t.Errorf("outsideTheFingerprint(%q) = %v, want %v", nodeType, got, want)
		}
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

// ---------- the wider capabilities ----------

// The three capabilities added when the built-ins became Lua — image, vision
// and agent — exist so that the privileged work can stay in Go while the node
// that asks for it is a file somebody can read. What makes that worth anything
// is that a pack which did not ask does not get them, and the tests below are
// that claim, checked from both sides of the boundary: the engine must not hand
// the functions over, and the sandbox must not put them on ctx.

// hostFunctionNames is every function a node's ctx can carry.
var hostFunctionNames = []string{
	"llm", "complete", "readFile", "writeFile",
	"generateImage", "editImage", "removeBackground",
	"rotateImage", "flipImage", "composeImages",
	"vision", "brain", "agentTools",
}

func reportedFunctions(t *testing.T, nodeType string, packs ...string) map[string]any {
	t.Helper()
	gemini := newFakeGemini(t)
	text := newScriptedOpenAI(t, assistantSays("never reached"))
	cfg := &providers.Config{
		TextProvider: "openai", OpenAIBaseURL: text.URL, OpenAIAPIKey: "test-key-not-a-real-one",
		GeminiBaseURL: gemini.URL, GoogleAPIKey: "test-key-not-a-real-one",
	}
	rt := NewRuntime("exec1", thenPreview(nodeType, nil), cfg, nil, nil)
	rt.Plugins = fixtureRegistry(t, packs...)
	rt.Files = newFakeFiles()
	if err := rt.Execute(context.Background()); err != nil {
		t.Fatalf("execute %s: %v", nodeType, err)
	}
	data, ok := rt.Outputs["B"].Value["data"].(map[string]any)
	if !ok {
		t.Fatalf("%s did not report a table: %+v", nodeType, rt.Outputs["B"])
	}
	return data
}

// TestAPackWithoutACapabilityHasNoneOfItsFunctions. Absent rather than
// present-and-failing, for every one of them: a function that exists and
// refuses lets a pack probe for the permission, branch on it, and behave when
// it is being examined.
func TestAPackWithoutACapabilityHasNoneOfItsFunctions(t *testing.T) {
	got := reportedFunctions(t, "reportCapabilities", "probe")
	for _, fn := range hostFunctionNames {
		if got[fn] != false {
			t.Errorf("ctx.%s was present for a pack that declared no capabilities", fn)
		}
	}
}

// TestDeclaringACapabilityIsWhatHandsTheFunctionsOver is the other half: a
// guarantee that nothing is granted is worth nothing if nothing is ever
// granted, and this is what says the gate is a gate rather than a wall.
func TestDeclaringACapabilityIsWhatHandsTheFunctionsOver(t *testing.T) {
	got := reportedFunctions(t, "reportEveryCapability", "powers")
	for _, fn := range hostFunctionNames {
		if got[fn] != true {
			t.Errorf("ctx.%s was missing from a pack that declared every capability", fn)
		}
	}
}

// TestTheEngineDoesNotHandOverWhatAPackDidNotDeclare is the same question one
// layer down, where it matters more: the sandbox checks the capability again
// before it exposes a function, but a pack that did not declare one must not be
// handed a live function at all, so that a bug in the far side of the boundary
// has nothing to leak.
func TestTheEngineDoesNotHandOverWhatAPackDidNotDeclare(t *testing.T) {
	reg := fixtureRegistry(t, "probe", "powers")
	rt := NewRuntime("exec1", &Graph{}, nil, nil, nil)
	rt.Plugins = reg
	rt.Files = newFakeFiles()

	for _, tc := range []struct {
		nodeType string
		want     bool
	}{
		{"reportCapabilities", false}, // its pack declares nothing
		{"reportEveryCapability", true},
	} {
		def, ok := reg.Kind(tc.nodeType)
		if !ok {
			t.Fatalf("%s is not registered", tc.nodeType)
		}
		host := rt.pluginHostInput(def, &RunInput{Node: &GraphNode{ID: "n", Type: tc.nodeType}, Config: map[string]any{}})
		// Kept as a slice of typed pairs rather than a map[string]any: a nil
		// func put into an interface is not a nil interface, so the obvious
		// version of this loop would have reported every function as handed
		// over and passed for the wrong reason.
		for _, fn := range []struct {
			name string
			f    plugins.NodeFunc
		}{
			{"Image.Generate", host.Image.Generate},
			{"Image.Edit", host.Image.Edit},
			{"Image.RemoveBackground", host.Image.RemoveBackground},
			{"Image.Rotate", host.Image.Rotate},
			{"Image.Flip", host.Image.Flip},
			{"Image.Compose", host.Image.Compose},
			{"Vision.Describe", host.Vision.Describe},
			{"Agent.Brain", host.Agent.Brain},
			{"Agent.Tools", host.Agent.Tools},
			{"Complete", host.Complete},
		} {
			if got := fn.f != nil; got != tc.want {
				t.Errorf("%s: %s was handed over = %v, want %v", tc.nodeType, fn.name, got, tc.want)
			}
		}
		if got := host.LLM != nil; got != tc.want {
			t.Errorf("%s: an LLMFunc was handed over = %v, want %v", tc.nodeType, got, tc.want)
		}
		if got := host.Files != nil; got != tc.want {
			t.Errorf("%s: a FileAccess was handed over = %v, want %v", tc.nodeType, got, tc.want)
		}
	}
}

// ---------- the budget ----------

// budgetRun runs one node of the powers pack with a chosen number of allowed
// provider calls, and reports how many actually reached a provider.
func budgetRun(t *testing.T, nodeType string, allowed int) (geminiCalls, textCalls int, err error) {
	t.Helper()
	gemini := newFakeGemini(t)
	text := newScriptedOpenAI(t, assistantSays("a model answer"))
	cfg := &providers.Config{
		TextProvider: "openai", OpenAIBaseURL: text.URL, OpenAIAPIKey: "test-key-not-a-real-one",
		GeminiBaseURL: gemini.URL, GoogleAPIKey: "test-key-not-a-real-one",
		ImageModel: "image-model", VisionModel: "vision-model",
	}
	graph := &Graph{
		Nodes: []GraphNode{
			imageSource("A", solidWithBorder(8, 8, white, blue)),
			node("B", nodeType, map[string]any{}),
			node("C", "preview", nil),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "B", "C")},
	}
	rt := NewRuntime("exec1", graph, cfg, nil, nil)
	rt.Plugins = fixtureRegistry(t, "powers")
	rt.PluginLimits = plugins.DefaultLimits()
	rt.PluginLimits.MaxLLMCalls = allowed

	err = rt.Execute(context.Background())
	return len(gemini.prompts), text.calls, err
}

// TestImageCallsCountAgainstTheModelBudget. They spend the user's own provider
// account, and an image costs more than a completion does, so a pack looping
// over ctx.generateImage is the most expensive mistake available to one.
func TestImageCallsCountAgainstTheModelBudget(t *testing.T) {
	gemini, _, err := budgetRun(t, "greedyImages", 3)
	if err == nil {
		t.Fatal("a node generating images in a loop was never stopped")
	}
	if !strings.Contains(err.Error(), "limit of 3") || !strings.Contains(err.Error(), "ctx.generateImage") {
		t.Errorf("the refusal does not name the function or the budget: %v", err)
	}
	if gemini != 3 {
		t.Fatalf("the budget let %d image calls through, expected 3", gemini)
	}
}

// TestEveryCapabilityDrawsOnTheOneBudget: separate allowances per capability
// would mean a node could spend five budgets instead of one, and the bill is
// the same bill whichever function ran up the charge.
func TestEveryCapabilityDrawsOnTheOneBudget(t *testing.T) {
	gemini, text, err := budgetRun(t, "mixedSpend", 4)
	if err == nil {
		t.Fatal("a node spending across three capabilities was never stopped")
	}
	// Four calls in total, in the order the script made them: llm, image,
	// vision, then llm again, and the fifth is refused.
	if got := gemini + text; got != 4 {
		t.Fatalf("%d provider calls were made against a budget of 4 (%d gemini, %d text)", got, gemini, text)
	}
}

// TestLocalImageWorkIsNotChargedToTheModelBudget. Turning and mirroring an
// image reaches no provider and costs nothing, and charging for it would mean a
// node that rotates six images cannot also call a model.
func TestLocalImageWorkIsNotChargedToTheModelBudget(t *testing.T) {
	_, text, err := budgetRun(t, "freeWork", 1)
	if err != nil {
		t.Fatalf("forty rotations and one model call did not fit in a budget of one model call: %v", err)
	}
	if text != 1 {
		t.Fatalf("the model was called %d times, want once", text)
	}
}

// TestABrainCannotBeAskedForUnboundedSteps. One ctx.brain call costs one unit
// of the model budget however many turns the loop takes, and the step count now
// arrives from a Lua node where nobody sees it — so the number a pack can ask
// for has to be bounded by something other than the person who did not type it.
func TestABrainCannotBeAskedForUnboundedSteps(t *testing.T) {
	text := newScriptedOpenAI(t, assistantCalls("call_1", "llm_T", `{"instructions":"again"}`))
	cfg := &providers.Config{
		TextProvider: "openai", OpenAIBaseURL: text.URL, OpenAIAPIKey: "test-key-not-a-real-one",
	}
	graph := &Graph{
		Nodes: []GraphNode{
			node("B", "brain", map[string]any{"goal": "loop forever", "maxSteps": 1000000}),
			node("T", "llm", map[string]any{"prompt": "x"}),
			node("P", "preview", nil),
		},
		Edges: []GraphEdge{dataEdge("e1", "B", "P"), toolEdge("t1", "T", "B")},
	}
	rt := NewRuntime("exec1", graph, cfg, nil, nil)
	if err := rt.Execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Two calls per step: the Brain's own turn and the llm tool it invokes.
	if got := text.calls; got > 2*brainMaxStepsCeiling {
		t.Fatalf("a Brain asked for a million steps made %d model calls; the ceiling of %d did not hold",
			got, brainMaxStepsCeiling)
	}
	if got := str(rt.Outputs["B"].Value["text"]); !strings.Contains(got, "maximum number of steps") {
		t.Errorf("the Brain did not stop at its ceiling: %q", got)
	}
}

// A node the engine can run must never be reported unknown. This is the test
// the comment on goImplementedNodeTypes points at: add a case to the switch in
// executeWithInput, forget to add it here, and the pre-flight refusal starts
// turning away workflows that would have run.
func TestNoNodeTheEngineCanRunIsReportedUnknown(t *testing.T) {
	var nodes []GraphNode
	for i, typ := range ReservedNodeTypes() {
		nodes = append(nodes, GraphNode{ID: fmt.Sprintf("n%d", i), Type: typ})
	}
	g := &Graph{Nodes: nodes}

	rt := NewRuntime("exec-known", g, nil, nil, nil)
	if unknown := rt.UnknownNodeTypes(g); len(unknown) != 0 {
		t.Fatalf("the engine reports its own node types as unknown: %v", unknown)
	}
}

func TestANodeNoPackProvidesIsReportedUnknown(t *testing.T) {
	g := &Graph{Nodes: []GraphNode{
		{ID: "n1", Type: "textInput"},
		{ID: "n2", Type: "summarize"},
		{ID: "n3", Type: "shout"},
		// Twice, to check the answer is a set and not one entry per node.
		{ID: "n4", Type: "shout"},
		{ID: "n5", Type: "output"},
	}}

	rt := NewRuntime("exec-unknown", g, nil, nil, nil)
	got := rt.UnknownNodeTypes(g)
	want := []string{"shout", "summarize"} // trié, pas dans l'ordre du graphe
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestUnknownNodeTypesOnNoGraph(t *testing.T) {
	if got := NewRuntime("exec-nil", nil, nil, nil, nil).UnknownNodeTypes(nil); got != nil {
		t.Fatalf("expected nothing for a nil graph, got %v", got)
	}
}
