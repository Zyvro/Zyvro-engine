package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/localstore"
)

// ---------- fakes ----------
//
// No test in this package makes a real model call or reaches a real provider.
// The package does not import providers at all, and the only LLM a node ever
// sees is the function the host hands it, which here is this one.

type fakeLLM struct {
	calls []LLMRequest
	reply string
	err   error
}

func (f *fakeLLM) complete(_ context.Context, req LLMRequest) (string, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return "", f.err
	}
	if f.reply == "" {
		return "fake answer", nil
	}
	return f.reply, nil
}

// recordingFiles wraps the real path gate so a test can see every path a script
// asked for, and still have the refusal come from the code that ships. Writing
// a second validator in the test would only prove the test agrees with itself.
type recordingFiles struct {
	inner  FileAccess
	reads  []string
	writes []string
}

func (r *recordingFiles) Read(rel string) ([]byte, string, error) {
	r.reads = append(r.reads, rel)
	return r.inner.Read(rel)
}

func (r *recordingFiles) Write(rel string, data []byte) (string, error) {
	r.writes = append(r.writes, rel)
	return r.inner.Write(rel, data)
}

// ---------- harness ----------

// oneNode loads a one-node pack and returns a registry holding it.
func oneNode(t *testing.T, manifest, src string) *Registry {
	t.Helper()
	p := loadOK(t, manifest, map[string]string{"node.lua": src})
	r := NewRegistry()
	if err := r.Install(p); err != nil {
		t.Fatalf("install: %v", err)
	}
	return r
}

func runNodeOK(t *testing.T, r *Registry, nodeType string, in HostInput) *Output {
	t.Helper()
	out, err := r.Run(context.Background(), nodeType, in)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out
}

func runNodeFails(t *testing.T, r *Registry, nodeType string, in HostInput, wants ...string) error {
	t.Helper()
	_, err := r.Run(context.Background(), nodeType, in)
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	return err
}

// ---------- ctx.input, ctx.config, ctx.log ----------

func TestNodeSeesItsInputAndConfig(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "echo",
			outputs = { "text" },
			run = function(ctx)
				ctx.log("saw " .. ctx.input.text)
				return { text = ctx.input.text .. "/" .. ctx.config.suffix .. "/" .. tostring(ctx.config.n) }
			end,
		}
	`)
	out, log, err := r.RunWithLog(context.Background(), "echo", HostInput{
		Input:  &Output{Type: "text", Value: map[string]any{"text": "hello"}},
		Config: map[string]any{"suffix": "world", "n": 7},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Type != "text" || out.Value["text"] != "hello/world/7" {
		t.Fatalf("unexpected output: %+v", out)
	}
	if len(log) != 1 || log[0] != "saw hello" {
		t.Fatalf("ctx.log did not reach the log: %#v", log)
	}
}

func TestUnconnectedInputIsAnEmptyTable(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "echo",
			outputs = { "text" },
			run = function(ctx) return { text = tostring(ctx.input.text) } end,
		}
	`)
	out := runNodeOK(t, r, "echo", HostInput{})
	if out.Value["text"] != "nil" {
		t.Fatalf("an unconnected input was not an empty table: %+v", out)
	}
}

func TestImageAndJSONInputsReachTheNode(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "shape",
			outputs = { "json" },
			run = function(ctx)
				if ctx.input.image then
					return { data = { kind = "image", mime = ctx.input.image.mimeType } }
				end
				return { data = { kind = "json", n = ctx.input.data.n } }
			end,
		}
	`)

	out := runNodeOK(t, r, "shape", HostInput{Input: &Output{Type: "image", Value: map[string]any{
		"mimeType": "image/png", "dataUrl": "data:image/png;base64,AAAA",
	}}})
	got := out.Value["data"].(map[string]any)
	if got["kind"] != "image" || got["mime"] != "image/png" {
		t.Fatalf("image input did not arrive: %#v", got)
	}

	out = runNodeOK(t, r, "shape", HostInput{Input: &Output{Type: "json", Value: map[string]any{
		"data": map[string]any{"n": 42.0},
	}}})
	got = out.Value["data"].(map[string]any)
	if got["kind"] != "json" || got["n"] != 42.0 {
		t.Fatalf("json input did not arrive: %#v", got)
	}
}

// ---------- ctx.llm ----------

// TestLLMBudgetIsEnforced is the money test: ctx.llm spends the user's own
// account, and a loop calling it is the most likely way a bad pack does real
// damage.
func TestLLMBudgetIsEnforced(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "greedy",
			outputs = { "text" },
			run = function(ctx)
				for i = 1, 1000 do ctx.llm{ prompt = "again" } end
				return { text = "never reached" }
			end,
		}
	`)
	llm := &fakeLLM{}
	limits := DefaultLimits()
	limits.MaxLLMCalls = 3

	err := runNodeFails(t, r, "greedy",
		HostInput{LLM: llm.complete, Limits: limits},
		"ctx.llm", "limit of 3")

	if len(llm.calls) != 3 {
		t.Fatalf("the budget let %d calls through, expected 3: %v", len(llm.calls), err)
	}
}

func TestLLMPassesTheRequestThrough(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "ask",
			outputs = { "text" },
			run = function(ctx)
				return { text = ctx.llm{
					prompt = "summarize this",
					system = "be terse",
					model = "some-model",
					maxTokens = 128,
				} }
			end,
		}
	`)
	llm := &fakeLLM{reply: "short"}
	out := runNodeOK(t, r, "ask", HostInput{LLM: llm.complete})

	if out.Value["text"] != "short" {
		t.Fatalf("the model answer did not come back: %+v", out)
	}
	if len(llm.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(llm.calls))
	}
	got := llm.calls[0]
	if got.Prompt != "summarize this" || got.System != "be terse" || got.Model != "some-model" || got.MaxTokens != 128 {
		t.Fatalf("request did not travel intact: %+v", got)
	}
}

// TestFailedLLMCallsStillCountAgainstTheBudget. Otherwise a node whose every
// call fails gets an unlimited number of them.
func TestFailedLLMCallsStillCountAgainstTheBudget(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "retry",
			outputs = { "text" },
			run = function(ctx)
				for i = 1, 100 do pcall(function() ctx.llm{ prompt = "x" } end) end
				return { text = "done" }
			end,
		}
	`)
	llm := &fakeLLM{err: fmt.Errorf("provider down")}
	limits := DefaultLimits()
	limits.MaxLLMCalls = 2

	runNodeOK(t, r, "retry", HostInput{LLM: llm.complete, Limits: limits})
	if len(llm.calls) != 2 {
		t.Fatalf("a retry loop made %d calls against a budget of 2", len(llm.calls))
	}
}

// TestLLMIsAbsentWithoutTheCapability.
func TestLLMIsAbsentWithoutTheCapability(t *testing.T) {
	r := oneNode(t, `{"name":"quiet","version":"1.0.0"}`, `
		return {
			type = "probe",
			outputs = { "text" },
			run = function(ctx) return { text = tostring(ctx.llm) } end,
		}
	`)
	out := runNodeOK(t, r, "probe", HostInput{LLM: (&fakeLLM{}).complete})
	if out.Value["text"] != "nil" {
		t.Fatalf("ctx.llm existed for a pack that did not declare it: %+v", out)
	}
}

// ---------- ctx.readFile / ctx.writeFile ----------

const filesManifest = `{"name":"files-pack","version":"1.0.0","capabilities":["files"]}`

// TestFileFunctionsAreAbsentWithoutTheCapability. Absent rather than
// present-and-failing: a function that exists and refuses lets a pack probe for
// the permission and behave differently when it does not have it.
func TestFileFunctionsAreAbsentWithoutTheCapability(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "probe",
			outputs = { "text" },
			run = function(ctx)
				return { text = tostring(ctx.readFile) .. "/" .. tostring(ctx.writeFile) }
			end,
		}
	`)
	files := &recordingFiles{inner: realFiles(t, t.TempDir())}
	out := runNodeOK(t, r, "probe", HostInput{Files: files})
	if out.Value["text"] != "nil/nil" {
		t.Fatalf("the file functions existed without the capability: %+v", out)
	}
	if len(files.reads)+len(files.writes) != 0 {
		t.Fatalf("the gate was touched: %v %v", files.reads, files.writes)
	}
}

func realFiles(t *testing.T, root string) FileAccess {
	t.Helper()
	store, err := localstore.Open(root)
	if err != nil {
		t.Fatalf("open project folder: %v", err)
	}
	return store.Files()
}

func TestReadFileGoesThroughTheGate(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("the contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := oneNode(t, filesManifest, `
		return {
			type = "reader",
			outputs = { "text" },
			run = function(ctx)
				local body, mime = ctx.readFile("notes.txt")
				return { text = body .. " (" .. mime .. ")" }
			end,
		}
	`)
	files := &recordingFiles{inner: realFiles(t, root)}
	out := runNodeOK(t, r, "reader", HostInput{Files: files})
	if !strings.HasPrefix(fmt.Sprint(out.Value["text"]), "the contents (text/plain") {
		t.Fatalf("readFile did not return the file: %+v", out)
	}
}

func TestWriteFileGoesThroughTheGate(t *testing.T) {
	root := t.TempDir()
	r := oneNode(t, filesManifest, `
		return {
			type = "writer",
			outputs = { "text" },
			run = function(ctx) return { text = ctx.writeFile("out/result.txt", "written by lua") } end,
		}
	`)
	files := &recordingFiles{inner: realFiles(t, root)}
	out := runNodeOK(t, r, "writer", HostInput{Files: files})
	if out.Value["text"] != "out/result.txt" {
		t.Fatalf("writeFile did not report the path: %+v", out)
	}
	body, err := os.ReadFile(filepath.Join(root, "out", "result.txt"))
	if err != nil || string(body) != "written by lua" {
		t.Fatalf("the file was not written: %v %q", err, body)
	}
}

// TestEscapingPathsAreRefusedByTheGate. The refusals come from
// localstore.FileAccess, not from anything in this package: there is one path
// validator in the product and this is a test that packs go through it.
func TestEscapingPathsAreRefusedByTheGate(t *testing.T) {
	root := t.TempDir()

	// A canary next to the project folder, so "no file outside the root was
	// opened" is checked against real bytes rather than against an error string.
	outside := filepath.Join(filepath.Dir(root), "canary-"+filepath.Base(root)+".txt")
	if err := os.WriteFile(outside, []byte("CANARY-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	// And a symlink inside the project that points at it, because a link
	// committed to a repository is part of the project to git and a way out of
	// it to the filesystem.
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}

	r := oneNode(t, filesManifest, `
		return {
			type = "thief",
			outputs = { "text" },
			run = function(ctx)
				local ok, err = pcall(function() return ctx.readFile(ctx.config.path) end)
				return { text = tostring(ok) .. ": " .. tostring(err) }
			end,
		}
	`)

	for _, path := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"/etc/hosts",
		"~/.ssh/id_rsa",
		"..",
		"../" + filepath.Base(outside),
		"link.txt",
		".zyvro/secrets.json",
	} {
		files := &recordingFiles{inner: realFiles(t, root)}
		out := runNodeOK(t, r, "thief", HostInput{
			Files:  files,
			Config: map[string]any{"path": path},
		})
		text := fmt.Sprint(out.Value["text"])
		if !strings.HasPrefix(text, "false: ") {
			t.Fatalf("%s was not refused: %s", path, text)
		}
		if strings.Contains(text, "CANARY-SECRET") {
			t.Fatalf("%s reached a file outside the project folder", path)
		}
		if len(files.reads) != 1 || files.reads[0] != path {
			t.Fatalf("%s: the gate saw %v", path, files.reads)
		}
	}
}

func TestEscapingWritesAreRefusedByTheGate(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(filepath.Dir(root), "escaped-"+filepath.Base(root)+".txt")
	t.Cleanup(func() { os.Remove(target) })

	r := oneNode(t, filesManifest, `
		return {
			type = "vandal",
			outputs = { "text" },
			run = function(ctx)
				local ok, err = pcall(function() return ctx.writeFile(ctx.config.path, "owned") end)
				return { text = tostring(ok) .. ": " .. tostring(err) }
			end,
		}
	`)

	for _, path := range []string{"../" + filepath.Base(target), target, ".zyvro/workflows/x.json"} {
		files := &recordingFiles{inner: realFiles(t, root)}
		out := runNodeOK(t, r, "vandal", HostInput{Files: files, Config: map[string]any{"path": path}})
		if !strings.HasPrefix(fmt.Sprint(out.Value["text"]), "false: ") {
			t.Fatalf("%s was not refused: %v", path, out.Value["text"])
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("a file was written outside the project folder: %v", err)
	}
}

func TestFileSizeIsCapped(t *testing.T) {
	root := t.TempDir()
	r := oneNode(t, filesManifest, `
		return {
			type = "writer",
			outputs = { "text" },
			run = function(ctx) return { text = ctx.writeFile("big.txt", ("x"):rep(200000)) } end,
		}
	`)
	limits := DefaultLimits()
	limits.MaxFileBytes = 1024
	runNodeFails(t, r, "writer", HostInput{Files: realFiles(t, root), Limits: limits},
		"ctx.writeFile", "over the limit")
}

// ---------- the result ----------

func TestOutputShapes(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "shaper",
			outputs = { "text", "image", "json" },
			run = function(ctx)
				if ctx.config.shape == "text" then return { text = "hi" } end
				if ctx.config.shape == "image" then
					return { image = { mimeType = "image/png", dataUrl = "data:image/png;base64,AAAA" } }
				end
				return { data = { a = 1, list = { "x", "y" } } }
			end,
		}
	`)

	out := runNodeOK(t, r, "shaper", HostInput{Config: map[string]any{"shape": "text"}})
	if out.Type != "text" || out.Value["text"] != "hi" {
		t.Fatalf("text output: %+v", out)
	}

	out = runNodeOK(t, r, "shaper", HostInput{Config: map[string]any{"shape": "image"}})
	if out.Type != "image" || out.Value["mimeType"] != "image/png" {
		t.Fatalf("image output: %+v", out)
	}

	out = runNodeOK(t, r, "shaper", HostInput{Config: map[string]any{"shape": "json"}})
	data, ok := out.Value["data"].(map[string]any)
	if out.Type != "json" || !ok || data["a"] != 1.0 {
		t.Fatalf("json output: %+v", out)
	}
	list, ok := data["list"].([]any)
	if !ok || len(list) != 2 || list[0] != "x" {
		t.Fatalf("a Lua array did not come back as one: %#v", data["list"])
	}
}

// TestImageOutputMustBeInline. An http URL here would be a network request made
// by the app, on the user's network, with a path the pack chose — a beacon for
// a thing that is supposed to have no network at all.
func TestImageOutputMustBeInline(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "beacon",
			outputs = { "image" },
			run = function(ctx)
				return { image = { mimeType = "image/png", dataUrl = ctx.config.url } }
			end,
		}
	`)
	for _, url := range []string{
		"https://evil.example/ping?pack=installed",
		"http://127.0.0.1:8080/admin",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"data:text/html;base64,AAAA",
	} {
		runNodeFails(t, r, "beacon", HostInput{Config: map[string]any{"url": url}}, "dataUrl")
	}
}

func TestBadReturnValuesAreExplained(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "confused",
			outputs = { "text" },
			run = function(ctx)
				if ctx.config.mode == "string" then return "just a string" end
				if ctx.config.mode == "empty" then return {} end
				return { text = function() end }
			end,
		}
	`)
	runNodeFails(t, r, "confused", HostInput{Config: map[string]any{"mode": "string"}}, "must return a table")
	runNodeFails(t, r, "confused", HostInput{Config: map[string]any{"mode": "empty"}}, "none of text, image or data")
	runNodeFails(t, r, "confused", HostInput{Config: map[string]any{"mode": "function"}}, "none of text, image or data")
}

// TestSelfReferencingResultIsRefused. A Lua table can point at itself, and a
// converter that followed it would recurse until the Go stack ran out.
func TestSelfReferencingResultIsRefused(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "ouroboros",
			outputs = { "json" },
			run = function(ctx)
				local t = {}
				t.self = t
				return { data = t }
			end,
		}
	`)
	runNodeFails(t, r, "ouroboros", HostInput{}, "nested")
}

// TestLuaErrorsComeBackNamingTheNode.
func TestLuaErrorsComeBackNamingTheNode(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "thrower",
			outputs = { "text" },
			run = function(ctx) error("something went wrong") end,
		}
	`)
	runNodeFails(t, r, "thrower", HostInput{}, "thrower", "something went wrong")
}

// TestLogSurvivesAFailedRun: a node that timed out after logging three lines is
// a node whose author needs those three lines.
func TestLogSurvivesAFailedRun(t *testing.T) {
	r := oneNode(t, goodManifest, `
		return {
			type = "chatty",
			outputs = { "text" },
			run = function(ctx)
				ctx.log("step one")
				ctx.log("step two")
				error("and then it broke")
			end,
		}
	`)
	_, log, err := r.RunWithLog(context.Background(), "chatty", HostInput{})
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	if len(log) != 2 || log[0] != "step one" {
		t.Fatalf("the log did not survive: %#v", log)
	}
}
