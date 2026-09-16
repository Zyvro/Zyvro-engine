package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/plugins"
)

// What a built-in is, and who is allowed to be one.
//
// This file used to parse Go. It had to: the list of names a pack could not
// take lived in the plugins package as a hand-written copy of the engine's
// dispatch switch, because plugins must not import engine, and nothing but a
// test reading both files as syntax could keep the copy in step. A name missing
// from it was a name a pack could take, and a pack defining "llm" would have
// been a very good attack that loaded cleanly.
//
// The copy is gone. The engine tells plugins what the reserved names are —
// ReservedNodeTypes is passed to NewRegistry and to Load — so there is one list
// and it lives where the knowledge is. That makes every check below a question
// about behaviour rather than about source text, which is the point: a test that
// reads a literal can only prove the literal says what it says.

// diff reports what is in want but not got, and the other way round, which is
// the only form of this failure anybody can act on.
func diff(got, want []string) (missing, extra []string) {
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

// everythingTheEngineShips is every node type a person can end up with in a
// graph without installing anything: the palette, plus the ones that are still
// dispatched but refuse to run.
func everythingTheEngineShips() []string {
	var out []string
	for _, k := range Catalogue(nil) {
		out = append(out, k.Type)
	}
	out = append(out, disabledNodeTypes...)
	sort.Strings(out)
	return out
}

// ---------- the bundled pack ----------

// TestTheBundledPackLoadsWithNoFilesystem. It is compiled into the binary, and
// that is a security property as much as a packaging one: a built-in read off
// disk is a built-in that anything with write access to that disk can replace.
// Running from an empty directory is how this says so — nothing under the
// working directory could supply these nodes, and they are all there anyway.
func TestTheBundledPackLoadsWithNoFilesystem(t *testing.T) {
	t.Chdir(t.TempDir())

	pack, err := plugins.LoadFS(builtinPackFS, "builtin", nil)
	if err != nil {
		t.Fatalf("the embedded pack does not load: %v", err)
	}
	if pack.Manifest.Name != BuiltinPackName {
		t.Fatalf("pack name = %q, want %q", pack.Manifest.Name, BuiltinPackName)
	}
	if len(pack.Nodes) == 0 {
		t.Fatal("the embedded pack contributed no nodes")
	}

	// And it runs, not merely parses: a node executed from a directory holding
	// nothing at all still produces its output.
	rt := NewRuntime("exec1", thenPreview("zyvroTools", nil), nil, nil, nil)
	rt.Plugins = NewRegistry()
	if err := rt.Execute(context.Background()); err != nil {
		t.Fatalf("a bundled node did not run with no filesystem under it: %v", err)
	}
}

// TestEveryBundledNodeDeclaresWhatThePaletteNeeds: these entries are served to
// the builder from GET /api/nodes and drawn from nothing else, so a node that
// forgets its label or its category is a node that looks broken on the canvas.
func TestEveryBundledNodeDeclaresWhatThePaletteNeeds(t *testing.T) {
	for _, k := range Catalogue(nil) {
		t.Run(k.Type, func(t *testing.T) {
			if k.Label == "" || k.Description == "" || k.Category == "" {
				t.Errorf("incomplete palette entry: %+v", k)
			}
			if k.Defaults == nil {
				t.Error("defaults is nil: the builder spreads it into a new node's config")
			}
			// The badge exists to say "this did not come from us". Putting it
			// on the nodes that did would make it mean nothing.
			if k.Pack != "" {
				t.Errorf("a node the engine ships is badged as coming from pack %q", k.Pack)
			}
		})
	}
}

// ---------- one implementation per node type ----------

// TestNothingIsBothAGoNodeAndALuaOne. The switch is tried first, so a type in
// both places would run the Go one and the Lua one would be dead code nobody
// could tell was dead — including whoever was editing it.
func TestNothingIsBothAGoNodeAndALuaOne(t *testing.T) {
	reg := NewRegistry()
	for _, nodeType := range BuiltinNodeTypes() {
		if _, ok := reg.Kind(nodeType); ok {
			t.Errorf("%q is dispatched in Go and also defined by the bundled pack; the Lua would never run", nodeType)
		}
	}
}

// TestEveryNodeTheEngineShipsActuallyDispatches: whatever the palette offers
// has to reach an implementation. A node that can be placed and then reports
// "unknown node type" is the failure this whole arrangement could produce
// silently — one rename in the bundled pack and a node disappears.
func TestEveryNodeTheEngineShipsActuallyDispatches(t *testing.T) {
	for _, nodeType := range everythingTheEngineShips() {
		t.Run(nodeType, func(t *testing.T) {
			// No providers, no files, no inputs: every node fails here, and
			// what matters is which failure. They fail for their own reasons —
			// "generate image needs a prompt", "video generation is disabled" —
			// and none of them may fail for not existing.
			rt := NewRuntime("exec1", thenPreview(nodeType, nil), nil, nil, nil)
			err := rt.Execute(context.Background())
			if err != nil && strings.Contains(err.Error(), "unknown node type") {
				t.Errorf("the palette offers %q but nothing runs it: %v", nodeType, err)
			}
		})
	}
}

// TestBuiltinNodeTypesIsTheGoHalf: BuiltinNodeTypes is what the engine says it
// implements itself, and it has to be the Go palette plus the disabled cases,
// not one of them. It is half of ReservedNodeTypes, and a name missing from it
// is a name a pack may take.
func TestBuiltinNodeTypesIsTheGoHalf(t *testing.T) {
	want := []string{"fileInput", "fileOutput", "generateVideo", "imageInput", "mergeText", "output", "preview", "textInput"}
	missing, extra := diff(BuiltinNodeTypes(), want)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("BuiltinNodeTypes is %v; missing %v, unexpected %v.\n"+
			"A node moved into or out of Go changes this list. If that was the intention, say so here.",
			BuiltinNodeTypes(), missing, extra)
	}
}

// ---------- what a pack may not take ----------

// TestNoPackCanTakeAnyNameTheEngineShips is the attack this arrangement exists
// to refuse, and the inversion is what makes it checkable in one loop: the
// names come from the engine, so the test cannot be looking at a stale copy.
//
// It covers the Lua built-ins as well as the Go ones, which the old source-
// reading test could not have: "llm" is now a file in the bundled pack, and a
// store pack claiming it has to be refused exactly as firmly as one claiming
// "textInput".
func TestNoPackCanTakeAnyNameTheEngineShips(t *testing.T) {
	reserved := ReservedNodeTypes()
	if len(reserved) <= len(BuiltinNodeTypes()) {
		t.Fatal("ReservedNodeTypes does not include the bundled pack's types, so a pack can shadow one")
	}
	for _, nodeType := range everythingTheEngineShips() {
		t.Run(nodeType, func(t *testing.T) {
			_, err := plugins.Load(packDefining(t, nodeType), reserved)
			if err == nil {
				t.Fatalf("a pack was allowed to define %q, which is a node the engine ships", nodeType)
			}
			if !strings.Contains(err.Error(), "built-in") {
				t.Errorf("the refusal does not say why: %v", err)
			}
		})
	}
}

// TestTheBundledPackCannotBeShadowedAtInstallEither. Load is the first gate and
// the one with the good message, but a pack that got past it — loaded against a
// shorter reserved list, or written before a node existed — must still not be
// able to replace a built-in in a live registry.
func TestTheBundledPackCannotBeShadowedAtInstallEither(t *testing.T) {
	for _, nodeType := range []string{"llm", "brain", "generateImage", "voxelPreview"} {
		t.Run(nodeType, func(t *testing.T) {
			// Loaded reserving nothing, which is the position a pack written
			// before this node existed would have been in.
			pack, err := plugins.Load(packDefining(t, nodeType), nil)
			if err != nil {
				t.Fatalf("fixture pack did not load: %v", err)
			}
			err = NewRegistry().Install(pack)
			if err == nil {
				t.Fatalf("a pack shadowing the built-in %q was installed", nodeType)
			}
			if !strings.Contains(err.Error(), "built-in") {
				t.Errorf("the refusal reads as a collision between two packs rather than as shadowing a built-in: %v", err)
			}
		})
	}
}

// packDefining writes a minimal pack whose one node claims a node type.
func packDefining(t *testing.T, nodeType string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nodes"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := []byte(`{"name":"shadow","version":"1.0.0"}`)
	if err := os.WriteFile(filepath.Join(dir, "zyvro-pack.json"), manifest, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	src := fmt.Sprintf("return { type = %q, outputs = { \"text\" }, run = function() return { text = \"x\" } end }", nodeType)
	if err := os.WriteFile(filepath.Join(dir, "nodes", "node.lua"), []byte(src), 0o644); err != nil {
		t.Fatalf("write node: %v", err)
	}
	return dir
}

// ---------- the palette's own promises ----------

// TestDisabledNodesAreNotInThePalette: nobody should be able to place a node
// that refuses to run. The dispatch keeps a case for it so that an old workflow
// still gets the real explanation instead of "unknown node type".
func TestDisabledNodesAreNotInThePalette(t *testing.T) {
	for _, k := range Catalogue(nil) {
		for _, disabled := range disabledNodeTypes {
			if k.Type == disabled {
				t.Errorf("%q refuses to run but the palette offers it", disabled)
			}
		}
	}
}

// TestLocalOnlyFlagAgreesWithLocalOnlyNodeTypes: the flag is what the builder
// badges and what the hosted server refuses a graph for, and the two have to be
// the same set or one of them is lying to somebody.
func TestLocalOnlyFlagAgreesWithLocalOnlyNodeTypes(t *testing.T) {
	flagged := []string{}
	for _, k := range Catalogue(nil) {
		if k.LocalOnly {
			flagged = append(flagged, k.Type)
		}
	}
	missing, extra := diff(flagged, LocalOnlyNodeTypes)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("localOnly in the palette disagrees with LocalOnlyNodeTypes: missing %v, extra %v", missing, extra)
	}
}

// TestCatalogueIsCopiedPerCall: a caller renders this and must not be able to
// reach back into the table every later caller is served from.
func TestCatalogueIsCopiedPerCall(t *testing.T) {
	first := BuiltinKinds()
	first[0].Label = "tampered"
	first[0].Defaults["injected"] = true
	first[0].Outputs[0] = "tampered"

	second := BuiltinKinds()
	if second[0].Label == "tampered" || second[0].Outputs[0] == "tampered" {
		t.Error("a caller mutated the shared palette")
	}
	if _, ok := second[0].Defaults["injected"]; ok {
		t.Error("a caller added a default to the shared palette")
	}

	// And the same for the half that comes from the bundled pack, which is
	// shared between every registry in the process.
	kinds := Catalogue(nil)
	for i := range kinds {
		kinds[i].Defaults["injected"] = true
	}
	for _, k := range Catalogue(nil) {
		if _, ok := k.Defaults["injected"]; ok {
			t.Fatalf("a caller reached into the bundled pack's definition of %q", k.Type)
		}
	}
}
