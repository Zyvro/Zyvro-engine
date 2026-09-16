package engine

import (
	"encoding/json"
	"testing"
)

func smallGraph() *Graph {
	// textInput "A" -> removeBackground "B" (config mode) -> preview "C"
	g := &Graph{}
	g.Nodes = []GraphNode{
		{ID: "A", Type: "textInput", Data: map[string]any{"label": "A", "config": map[string]any{"value": "hello"}}},
		{ID: "B", Type: "removeBackground", Data: map[string]any{"label": "B", "config": map[string]any{"mode": "programmatic"}}},
		{ID: "C", Type: "preview"},
	}
	g.Edges = []GraphEdge{
		{ID: "e1", Source: "A", Target: "B", SourceHandle: "out", TargetHandle: "text", Type: "data"},
		{ID: "e2", Source: "B", Target: "C", SourceHandle: "out", TargetHandle: "in", Type: "data"},
	}
	return g
}

func TestFingerprintStable(t *testing.T) {
	g := smallGraph()
	fps1, parents, err := ComputeFingerprints(g, nil)
	if err != nil {
		t.Fatal(err)
	}
	fps2, _, err := ComputeFingerprints(g, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fps1) != 3 {
		t.Fatalf("expected 3 fingerprints, got %d", len(fps1))
	}
	for id, fp := range fps1 {
		if fps2[id] != fp {
			t.Fatalf("fingerprint of %s not stable", id)
		}
	}
	// parents: A has none, B has A, C has B.
	if len(parents["A"]) != 0 || len(parents["B"]) != 1 || parents["B"][0] != "A" || parents["C"][0] != "B" {
		t.Fatalf("parents wrong: %v", parents)
	}
}

func TestFingerprintConfigChangeInvalidatesDownstream(t *testing.T) {
	g := smallGraph()
	fpsBefore, _, err := ComputeFingerprints(g, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Change A's config: A, B and C fingerprints must all change.
	cfg := g.Nodes[0].Data["config"].(map[string]any)
	cfg["value"] = "changed"
	fpsAfter, _, err := ComputeFingerprints(g, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"A", "B", "C"} {
		if fpsBefore[id] == fpsAfter[id] {
			t.Fatalf("fingerprint of %s must change when upstream config changes", id)
		}
	}

	// Changing a node's own config only invalidates itself + downstream.
	// Revert A, change B.
	cfg["value"] = "hello"
	bcfg := g.Nodes[1].Data["config"].(map[string]any)
	bcfg["mode"] = "ai"
	fpsB, _, _ := ComputeFingerprints(g, nil)
	if fpsB["A"] != fpsBefore["A"] {
		t.Fatal("A fingerprint must be unchanged")
	}
	if fpsB["B"] == fpsBefore["B"] {
		t.Fatal("B fingerprint must change")
	}
	if fpsB["C"] == fpsBefore["C"] {
		t.Fatal("C fingerprint must change (downstream of B)")
	}
}

func TestLoadCacheAndReplay(t *testing.T) {
	g := smallGraph()
	fps, _, err := ComputeFingerprints(g, nil)
	if err != nil {
		t.Fatal(err)
	}

	out := imageOutput("image/png", []byte("fakepng"))
	outJSON, _ := json.Marshal(out)
	cached := []CachedNodeResult{
		{NodeID: "B", ExecutionID: "exec-old", Status: "completed", Fingerprint: string(fps["B"]), OutputJSON: string(outJSON)},
		{NodeID: "A", ExecutionID: "exec-old", Status: "failed", Fingerprint: string(fps["A"]), OutputJSON: string(outJSON)},
	}
	cache := LoadCache(cached)
	if len(cache) != 1 {
		t.Fatalf("failed nodes must not be cached, got %d entries", len(cache))
	}
	entry, ok := cache["B"]
	if !ok || entry.Output == nil || entry.Output.Type != "image" {
		t.Fatalf("cache entry B wrong: %+v", entry)
	}

	// A node replayed in the previous run ("cached" status) carries the same
	// fingerprinted output and must remain replayable on the next run.
	cacheWithCachedStatus := LoadCache([]CachedNodeResult{
		{NodeID: "C", ExecutionID: "exec-old", Status: "cached", Fingerprint: string(fps["C"]), OutputJSON: string(outJSON)},
	})
	entry, ok = cacheWithCachedStatus["C"]
	if !ok || entry.Output == nil {
		t.Fatal("a 'cached' node execution must be a valid cache entry")
	}
	rt2 := NewRuntime("exec-mid", g, nil, nil, nil)
	rt2.Fingerprints = fps
	rt2.Cache = cacheWithCachedStatus
	if _, ok := rt2.cachedOutput("C"); !ok {
		t.Fatal("C must be replayable from a 'cached' previous status")
	}

	rt := NewRuntime("exec-new", g, nil, nil, nil)
	rt.Fingerprints = fps
	rt.Cache = cache
	entry, ok = rt.cachedOutput("B")
	if !ok {
		t.Fatal("B must be replayable from cache")
	}
	if _, ok := rt.cachedOutput("A"); ok {
		t.Fatal("A must not be replayable (failed in the previous run)")
	}
}