package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodManifest = `{
  "name": "demo-pack",
  "version": "1.0.0",
  "description": "A pack for the tests",
  "author": "nobody",
  "capabilities": ["llm"]
}`

const summarizeNode = `
return {
  type = "summarize",
  label = "Summarize",
  category = "AI",
  description = "Shorten some text",
  inputs = { "text" },
  outputs = { "text" },
  config = {
    { key = "sentences", label = "Sentences", type = "number", default = 3 },
  },
  run = function(ctx)
    return { text = "summary: " .. (ctx.input.text or "") }
  end,
}
`

// writePack lays out a pack directory. A nil manifest means the file is absent,
// which is its own test case.
func writePack(t *testing.T, manifest string, nodes map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, nodesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range nodes {
		if err := os.WriteFile(filepath.Join(dir, nodesDir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// testReserved stands in for what a host passes: this package no longer holds a
// list of built-in names, so the tests say which names are taken the same way
// the engine does — by handing them to Load.
var testReserved = []string{"brain", "fileInput", "llm", "textInput"}

func loadOK(t *testing.T, manifest string, nodes map[string]string) *Pack {
	t.Helper()
	p, err := Load(writePack(t, manifest, nodes), testReserved)
	if err != nil {
		t.Fatalf("pack did not load: %v", err)
	}
	return p
}

func loadFails(t *testing.T, manifest string, nodes map[string]string, wants ...string) error {
	t.Helper()
	_, err := Load(writePack(t, manifest, nodes), testReserved)
	if err == nil {
		t.Fatal("expected the pack to be refused")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	return err
}

func TestLoadReadsADefinition(t *testing.T) {
	p := loadOK(t, goodManifest, map[string]string{"summarize.lua": summarizeNode})

	if p.Manifest.Name != "demo-pack" || p.Manifest.Version != "1.0.0" {
		t.Fatalf("manifest not read: %+v", p.Manifest)
	}
	if len(p.Nodes) != 1 {
		t.Fatalf("expected one node, got %d", len(p.Nodes))
	}
	def := p.Nodes[0]
	if def.Type != "summarize" || def.Label != "Summarize" || def.Category != "AI" {
		t.Fatalf("definition not read: %+v", def)
	}
	if len(def.Inputs) != 1 || def.Inputs[0] != "text" || len(def.Outputs) != 1 || def.Outputs[0] != "text" {
		t.Fatalf("ports not read: %+v", def)
	}
	if len(def.Config) != 1 || def.Config[0].Key != "sentences" || def.Config[0].Type != "number" {
		t.Fatalf("config not read: %+v", def.Config)
	}
	if got, ok := def.Config[0].Default.(float64); !ok || got != 3 {
		t.Fatalf("config default not read: %#v", def.Config[0].Default)
	}
	if def.Pack != "demo-pack" || !def.Has(CapLLM) || def.Has(CapFiles) {
		t.Fatalf("capabilities did not travel with the node: %+v", def)
	}
}

func TestLoadFillsInOptionalFields(t *testing.T) {
	p := loadOK(t, goodManifest, map[string]string{"bare.lua": `
		return {
			type = "bare",
			outputs = { "text" },
			run = function(ctx) return { text = "hi" } end,
		}
	`})
	def := p.Nodes[0]
	if def.Label != "bare" || def.Category != "Utility" {
		t.Fatalf("defaults not applied: %+v", def)
	}
}

// TestNodeCollidingWithABuiltinIsRejected. Shadowing "llm" would be a very good
// attack: every workflow that already uses it would start running pack code.
//
// Which names are built-in is the host's to say — this package is told, it does
// not know — so what is checked here is that being told is enough.
func TestNodeCollidingWithABuiltinIsRejected(t *testing.T) {
	for _, builtin := range testReserved {
		loadFails(t, goodManifest, map[string]string{"evil.lua": `
			return {
				type = "` + builtin + `",
				outputs = { "text" },
				run = function(ctx) return { text = "pwned" } end,
			}
		`}, builtin, "built-in")
	}
}

func TestMalformedManifestIsRejected(t *testing.T) {
	loadFails(t, `{ "name": "demo", `, map[string]string{"a.lua": summarizeNode},
		manifestName, "not valid")

	loadFails(t, `{"name": "Demo Pack", "version": "1.0.0"}`,
		map[string]string{"a.lua": summarizeNode}, "name", "Demo Pack")

	loadFails(t, `{"name": "demo", "version": ""}`,
		map[string]string{"a.lua": summarizeNode}, "version")

	loadFails(t, `{"name": "demo", "version": "1.0.0", "permissions": ["all"]}`,
		map[string]string{"a.lua": summarizeNode}, "permissions")
}

func TestMissingManifestIsRejected(t *testing.T) {
	loadFails(t, "", map[string]string{"a.lua": summarizeNode}, manifestName)
}

// TestTomlManifestSaysWhatIsSupported. The brief allowed either; JSON is what
// got built, so a pack shipping TOML deserves to be told that rather than "no
// manifest here".
func TestTomlManifestSaysWhatIsSupported(t *testing.T) {
	dir := writePack(t, "", map[string]string{"a.lua": summarizeNode})
	if err := os.WriteFile(filepath.Join(dir, altManifestName), []byte("name = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir, testReserved)
	if err == nil || !strings.Contains(err.Error(), altManifestName) || !strings.Contains(err.Error(), manifestName) {
		t.Fatalf("a TOML manifest was not explained: %v", err)
	}
}

func TestUnknownCapabilityIsRejected(t *testing.T) {
	loadFails(t, `{"name":"demo","version":"1.0.0","capabilities":["network"]}`,
		map[string]string{"a.lua": summarizeNode}, "capability", "network")
}

func TestBadNodeTypeNameIsRejected(t *testing.T) {
	for _, bad := range []string{"Summarize", "1node", "my-node", "my node", "", strings.Repeat("a", 65)} {
		loadFails(t, goodManifest, map[string]string{"a.lua": `
			return {
				type = "` + bad + `",
				outputs = { "text" },
				run = function(ctx) return { text = "x" } end,
			}
		`}, "type")
	}
}

func TestUnknownPortTypeIsRejected(t *testing.T) {
	loadFails(t, goodManifest, map[string]string{"a.lua": `
		return {
			type = "thing",
			inputs = { "video" },
			outputs = { "text" },
			run = function(ctx) return { text = "x" } end,
		}
	`}, "inputs", "video")

	loadFails(t, goodManifest, map[string]string{"a.lua": `
		return {
			type = "thing",
			outputs = { "sound" },
			run = function(ctx) return { text = "x" } end,
		}
	`}, "outputs", "sound")
}

func TestUnknownCategoryIsRejected(t *testing.T) {
	loadFails(t, goodManifest, map[string]string{"a.lua": `
		return {
			type = "thing",
			category = "Mischief",
			outputs = { "text" },
			run = function(ctx) return { text = "x" } end,
		}
	`}, "category", "Mischief")
}

func TestBadConfigIsRejected(t *testing.T) {
	loadFails(t, goodManifest, map[string]string{"a.lua": `
		return {
			type = "thing", outputs = { "text" },
			config = { { key = "a b", type = "text" } },
			run = function(ctx) return { text = "x" } end,
		}
	`}, "key")

	loadFails(t, goodManifest, map[string]string{"a.lua": `
		return {
			type = "thing", outputs = { "text" },
			config = { { key = "mode", type = "colour" } },
			run = function(ctx) return { text = "x" } end,
		}
	`}, "type", "colour")

	loadFails(t, goodManifest, map[string]string{"a.lua": `
		return {
			type = "thing", outputs = { "text" },
			config = { { key = "mode", type = "select" } },
			run = function(ctx) return { text = "x" } end,
		}
	`}, "options")

	loadFails(t, goodManifest, map[string]string{"a.lua": `
		return {
			type = "thing", outputs = { "text" },
			config = { { key = "a", type = "text" }, { key = "a", type = "text" } },
			run = function(ctx) return { text = "x" } end,
		}
	`}, "twice")
}

func TestNodeWithoutRunIsRejected(t *testing.T) {
	loadFails(t, goodManifest, map[string]string{"a.lua": `
		return { type = "thing", outputs = { "text" } }
	`}, "run")
}

func TestNodeThatReturnsNothingIsRejected(t *testing.T) {
	loadFails(t, goodManifest, map[string]string{"a.lua": `local x = 1`}, "return")
}

func TestDuplicateNodeTypeInOnePackIsRejected(t *testing.T) {
	loadFails(t, goodManifest, map[string]string{
		"one.lua": summarizeNode,
		"two.lua": summarizeNode,
	}, "summarize", "already defined")
}

// TestLoadingIsSandboxed. Evaluating a definition must not be able to do work:
// that is why it happens in the same sandbox, with a shorter deadline.
func TestLoadingIsSandboxed(t *testing.T) {
	// The loader's own state is the run state: the libraries a pack could use
	// to do work at load time are simply not there.
	loadOK(t, goodManifest, map[string]string{"a.lua": `
		for _, name in ipairs({"io", "os", "package", "debug", "coroutine", "require", "load", "dofile"}) do
			assert(_G[name] == nil, name .. " is reachable while a pack is loading")
		end
		return {
			type = "thing",
			outputs = { "text" },
			run = function(ctx) return { text = "x" } end,
		}
	`})

	loadFails(t, goodManifest, map[string]string{"a.lua": `
		local f = io.open("/etc/passwd")
		return { type = "thing", outputs = { "text" }, run = function() end }
	`}, "attempt to index", "open")

	loadFails(t, goodManifest, map[string]string{"a.lua": `
		while true do end
	`}, "ran longer than")
}

// TestBytecodeNodeFileIsRejectedAtLoad.
func TestBytecodeNodeFileIsRejectedAtLoad(t *testing.T) {
	loadFails(t, goodManifest, map[string]string{"a.lua": "\x1bLua\x51 whatever"}, "bytecode")
}

func TestPackWithNoNodesIsRejected(t *testing.T) {
	loadFails(t, goodManifest, map[string]string{}, "no nodes")
}

// TestNonLuaFilesAreIgnored: a pack may ship examples and a readme.
func TestNonLuaFilesAreIgnored(t *testing.T) {
	p := loadOK(t, goodManifest, map[string]string{
		"summarize.lua": summarizeNode,
		"README.md":     "# not a node",
	})
	if len(p.Nodes) != 1 {
		t.Fatalf("expected one node, got %d", len(p.Nodes))
	}
}
