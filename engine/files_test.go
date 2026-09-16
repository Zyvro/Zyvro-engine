package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"reflect"
	"strings"
	"testing"
)

// fakeFiles is a project folder in memory. The engine's half of this feature is
// entirely about what it does with the bytes, so the tests give it bytes
// directly instead of a temp directory; the real path handling is what
// localstore/fileaccess_test.go is for.
type fakeFiles struct {
	read  map[string][]byte
	mimes map[string]string
	wrote map[string][]byte
	// dirs records the createDirs flag of the last write, so the test can see
	// that the node's config actually reached the filesystem.
	dirs bool
	// reads records every path asked for, in order, so a test can check which
	// path reached the gate rather than only what came back.
	reads    []string
	readErr  error
	writeErr error
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{
		read:  map[string][]byte{},
		mimes: map[string]string{},
		wrote: map[string][]byte{},
	}
}

func (f *fakeFiles) Read(rel string) ([]byte, string, error) {
	f.reads = append(f.reads, rel)
	if f.readErr != nil {
		return nil, "", f.readErr
	}
	data, ok := f.read[rel]
	if !ok {
		return nil, "", fmt.Errorf("no such file in the project folder")
	}
	return data, f.mimes[rel], nil
}

func (f *fakeFiles) Write(rel string, data []byte) (string, error) {
	return f.WriteWithDirs(rel, data, true)
}

func (f *fakeFiles) WriteWithDirs(rel string, data []byte, createDirs bool) (string, error) {
	if f.writeErr != nil {
		return "", f.writeErr
	}
	f.dirs = createDirs
	f.wrote[rel] = data
	return rel, nil
}

func fileNode(t *testing.T, typ string, cfg map[string]any) *GraphNode {
	t.Helper()
	return &GraphNode{ID: "n1", Type: typ, Data: map[string]any{"config": cfg}}
}

// runFile executes one file node the way the DAG loop does, through the
// dispatch switch, so a node that is not wired in fails these tests.
func runFile(t *testing.T, rt *Runtime, typ string, cfg map[string]any, upstream ...*NodeOutput) (*NodeOutput, error) {
	t.Helper()
	n := fileNode(t, typ, cfg)
	return rt.executeWithInput(context.Background(), &RunInput{Node: n, Upstream: upstream, Config: nodeConfig(n)})
}

// samplePNG is a real PNG, so both the extension and the bytes say "image".
func samplePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	data, err := encodePNG(img)
	if err != nil {
		t.Fatalf("encode sample png: %v", err)
	}
	return data
}

// What a file becomes with no "as" set is the thing an author will hit most
// often, so every extension the auto mode claims to know is pinned here.
func TestFileInputAutoDetectsKindFromExtension(t *testing.T) {
	png := samplePNG(t)
	cases := []struct {
		path string
		mime string
		data []byte
		want string
	}{
		{"notes.txt", "text/plain", []byte("hello"), "text"},
		{"README.md", "text/markdown", []byte("# hi"), "text"},
		{"prompt", "", []byte("no extension at all"), "text"},
		{"data/config.json", "application/json", []byte(`{"a":1}`), "json"},
		{"assets/photo.png", "image/png", png, "image"},
		{"assets/PHOTO.JPG", "image/jpeg", png, "image"},
		{"assets/logo.webp", "image/webp", png, "image"},
		// No useful extension, but the store knows what it is.
		{"assets/blob", "image/png", png, "image"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			files := newFakeFiles()
			files.read[tc.path] = tc.data
			files.mimes[tc.path] = tc.mime
			rt := &Runtime{Files: files}

			out, err := runFile(t, rt, "fileInput", map[string]any{"path": tc.path})
			if err != nil {
				t.Fatalf("fileInput: %v", err)
			}
			if out.Type != tc.want {
				t.Fatalf("type = %q, want %q", out.Type, tc.want)
			}
		})
	}
}

// The image output is a contract with vision, editImage and removeBackground:
// they read dataUrl out of it and would not notice the node that produced it.
// So it has to be the very same map imageInput builds.
func TestFileInputImageMatchesImageInputExactly(t *testing.T) {
	png := samplePNG(t)
	files := newFakeFiles()
	files.read["assets/photo.png"] = png
	files.mimes["assets/photo.png"] = "image/png"
	rt := &Runtime{Files: files, Inputs: map[string]any{}}

	fromFile, err := runFile(t, rt, "fileInput", map[string]any{"path": "assets/photo.png"})
	if err != nil {
		t.Fatalf("fileInput: %v", err)
	}
	n := fileNode(t, "imageInput", map[string]any{"dataUrl": imageDataURL("image/png", png)})
	fromImageInput, err := rt.runImageInput(&RunInput{Node: n, Config: nodeConfig(n)})
	if err != nil {
		t.Fatalf("imageInput: %v", err)
	}
	if fromFile.Type != fromImageInput.Type || !reflect.DeepEqual(fromFile.Value, fromImageInput.Value) {
		t.Fatalf("file image output %+v does not match the image input shape %+v", fromFile, fromImageInput)
	}
}

// "as" overrides the extension, which is how someone reads a .json as raw text
// or an extensionless dump as an image.
func TestFileInputExplicitModes(t *testing.T) {
	png := samplePNG(t)

	t.Run("json read as text stays a string", func(t *testing.T) {
		files := newFakeFiles()
		files.read["a.json"] = []byte(`{"a":1}`)
		rt := &Runtime{Files: files}
		out, err := runFile(t, rt, "fileInput", map[string]any{"path": "a.json", "as": "text"})
		if err != nil || out.Type != "text" || out.Value["text"] != `{"a":1}` {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
	})

	t.Run("text read as json is parsed", func(t *testing.T) {
		files := newFakeFiles()
		files.read["a.txt"] = []byte(`{"a":1}`)
		rt := &Runtime{Files: files}
		out, err := runFile(t, rt, "fileInput", map[string]any{"path": "a.txt", "as": "json"})
		if err != nil || out.Type != "json" {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		m, ok := out.Value["data"].(map[string]any)
		if !ok || m["a"] != float64(1) {
			t.Fatalf("data = %#v", out.Value["data"])
		}
	})

	t.Run("bytes decide when the name does not", func(t *testing.T) {
		files := newFakeFiles()
		files.read["dump"] = png
		rt := &Runtime{Files: files}
		out, err := runFile(t, rt, "fileInput", map[string]any{"path": "dump", "as": "image"})
		if err != nil || out.Type != "image" || out.Value["mimeType"] != "image/png" {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
	})

	t.Run("invalid json is refused, not passed on", func(t *testing.T) {
		files := newFakeFiles()
		files.read["a.json"] = []byte("not json at all")
		rt := &Runtime{Files: files}
		if _, err := runFile(t, rt, "fileInput", map[string]any{"path": "a.json"}); err == nil ||
			!strings.Contains(err.Error(), "not valid JSON") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("text is not an image", func(t *testing.T) {
		files := newFakeFiles()
		files.read["a.txt"] = []byte("hello")
		rt := &Runtime{Files: files}
		if _, err := runFile(t, rt, "fileInput", map[string]any{"path": "a.txt", "as": "image"}); err == nil ||
			!strings.Contains(err.Error(), "not an image") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an unknown mode is a mistake worth reporting", func(t *testing.T) {
		files := newFakeFiles()
		files.read["a.txt"] = []byte("hello")
		rt := &Runtime{Files: files}
		if _, err := runFile(t, rt, "fileInput", map[string]any{"path": "a.txt", "as": "csv"}); err == nil ||
			!strings.Contains(err.Error(), "unknown mode") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a path is required", func(t *testing.T) {
		rt := &Runtime{Files: newFakeFiles()}
		if _, err := runFile(t, rt, "fileInput", map[string]any{}); err == nil ||
			!strings.Contains(err.Error(), "needs a path") {
			t.Fatalf("err = %v", err)
		}
	})
}

// A file node's path can be driven by a run input, which is what turns one
// workflow into a batch over a folder.
func TestFileInputResolvesRuntimeInputsInThePath(t *testing.T) {
	files := newFakeFiles()
	files.read["assets/cat.png"] = samplePNG(t)
	files.mimes["assets/cat.png"] = "image/png"
	rt := &Runtime{Files: files, Inputs: map[string]any{"name": "cat"}}

	out, err := runFile(t, rt, "fileInput", map[string]any{"path": "assets/{{input:name}}.png"})
	if err != nil {
		t.Fatalf("fileInput: %v", err)
	}
	if out.Type != "image" {
		t.Fatalf("out = %+v", out)
	}
}

// Everything a node produces is held in memory as a base64 data URL, so the
// size limit is what keeps a stray video from killing the process. The message
// has to say how big the file actually was, or the author cannot tell whether
// they picked the wrong file or need a different tool.
func TestFileInputRefusesAFileOverTheLimit(t *testing.T) {
	files := newFakeFiles()
	files.read["big.bin"] = make([]byte, 30<<20)
	files.mimes["big.bin"] = "application/octet-stream"
	rt := &Runtime{Files: files}

	_, err := runFile(t, rt, "fileInput", map[string]any{"path": "big.bin"})
	if err == nil {
		t.Fatal("a 30 MB file must be refused")
	}
	if !strings.Contains(err.Error(), "30.0 MB") || !strings.Contains(err.Error(), "25.0 MB") {
		t.Fatalf("the refusal must name both sizes: %v", err)
	}
}

// Decoding a binary file as text used to be the worst kind of failure: a
// successful run whose prompt was full of replacement characters.
func TestFileInputRefusesBinaryReadAsText(t *testing.T) {
	cases := map[string][]byte{
		"invalid UTF-8": {0xff, 0xfe, 0x41, 0x42},
		"NUL bytes":     {'M', 'Z', 0x00, 0x00, 'x'},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			files := newFakeFiles()
			files.read["a.bin"] = data
			rt := &Runtime{Files: files}
			out, err := runFile(t, rt, "fileInput", map[string]any{"path": "a.bin", "as": "text"})
			if err == nil {
				t.Fatalf("binary read as text must fail, got %+v", out)
			}
			if strings.ContainsRune(err.Error(), '�') {
				t.Fatalf("the error itself leaked replacement characters: %v", err)
			}
			if !strings.Contains(err.Error(), "a.bin") {
				t.Fatalf("the error must name the file: %v", err)
			}
		})
	}
}

// The written file is the point, but the passthrough is what lets a fileOutput
// sit in the middle of a chain instead of only at its end.
func TestFileOutputWritesAndPassesTheValueThrough(t *testing.T) {
	files := newFakeFiles()
	rt := &Runtime{Files: files}
	src := textOutput("hello world")

	out, err := runFile(t, rt, "fileOutput", map[string]any{"path": "out/note.txt"}, src)
	if err != nil {
		t.Fatalf("fileOutput: %v", err)
	}
	if string(files.wrote["out/note.txt"]) != "hello world" {
		t.Fatalf("wrote %q", files.wrote["out/note.txt"])
	}
	if out.Type != "text" || out.Value["text"] != "hello world" {
		t.Fatalf("the value did not survive the node: %+v", out)
	}
	if out.Value["savedPath"] != "out/note.txt" {
		t.Fatalf("savedPath = %v", out.Value["savedPath"])
	}
	// The upstream node's own output is shared with everything else reading it,
	// so it must come back out of here untouched.
	if _, leaked := src.Value["savedPath"]; leaked {
		t.Fatal("fileOutput mutated its input instead of copying it")
	}
}

func TestFileOutputEncodesEachType(t *testing.T) {
	png := samplePNG(t)

	t.Run("an image writes its decoded bytes", func(t *testing.T) {
		files := newFakeFiles()
		rt := &Runtime{Files: files}
		out, err := runFile(t, rt, "fileOutput", map[string]any{"path": "out/a.png"}, imageOutput("image/png", png))
		if err != nil {
			t.Fatalf("fileOutput: %v", err)
		}
		if !reflect.DeepEqual(files.wrote["out/a.png"], png) {
			t.Fatal("the file on disk is not the image that went in")
		}
		if out.Value["savedPath"] != "out/a.png" || out.Value["dataUrl"] == nil {
			t.Fatalf("out = %+v", out)
		}
	})

	t.Run("json is written indented, with a trailing newline", func(t *testing.T) {
		files := newFakeFiles()
		rt := &Runtime{Files: files}
		if _, err := runFile(t, rt, "fileOutput", map[string]any{"path": "out/a.json"},
			jsonOutput(map[string]any{"b": 2, "a": 1})); err != nil {
			t.Fatalf("fileOutput: %v", err)
		}
		written := string(files.wrote["out/a.json"])
		if !strings.HasSuffix(written, "}\n") || !strings.Contains(written, "\n  \"a\": 1") {
			t.Fatalf("written json = %q", written)
		}
		var back map[string]any
		if err := json.Unmarshal([]byte(written), &back); err != nil {
			t.Fatalf("what we wrote does not parse: %v", err)
		}
	})

	t.Run("the first non-nil upstream wins", func(t *testing.T) {
		files := newFakeFiles()
		rt := &Runtime{Files: files}
		if _, err := runFile(t, rt, "fileOutput", map[string]any{"path": "out/a.txt"},
			nil, textOutput("second")); err != nil {
			t.Fatalf("fileOutput: %v", err)
		}
		if string(files.wrote["out/a.txt"]) != "second" {
			t.Fatalf("wrote %q", files.wrote["out/a.txt"])
		}
	})
}

func TestFileOutputNeedsSomethingToWrite(t *testing.T) {
	rt := &Runtime{Files: newFakeFiles()}
	_, err := runFile(t, rt, "fileOutput", map[string]any{"path": "out/a.txt"})
	if err == nil || !strings.Contains(err.Error(), "nothing to write") {
		t.Fatalf("err = %v", err)
	}
}

// createDirs defaults on, because the common case is writing into an out/
// folder that does not exist yet; turning it off has to actually reach the
// store, not be read and dropped.
func TestFileOutputCreateDirsReachesTheStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want bool
	}{
		{"unset means yes", map[string]any{"path": "out/a.txt"}, true},
		{"explicitly off", map[string]any{"path": "out/a.txt", "createDirs": false}, false},
		{"explicitly on", map[string]any{"path": "out/a.txt", "createDirs": true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := newFakeFiles()
			rt := &Runtime{Files: files}
			if _, err := runFile(t, rt, "fileOutput", tc.cfg, textOutput("x")); err != nil {
				t.Fatalf("fileOutput: %v", err)
			}
			if files.dirs != tc.want {
				t.Fatalf("createDirs reached the store as %v, want %v", files.dirs, tc.want)
			}
		})
	}
}

// With no project folder there is nothing to refuse access to: the capability
// is simply absent, and both nodes say so in the same words.
func TestFileNodesRefuseWithoutAProjectFolder(t *testing.T) {
	const want = "this node reads and writes files in your project folder, so it only runs in Zyvro Studio, the desktop app"
	rt := &Runtime{}

	if _, err := runFile(t, rt, "fileInput", map[string]any{"path": "a.txt"}); err == nil || err.Error() != want {
		t.Fatalf("fileInput error = %v", err)
	}
	if _, err := runFile(t, rt, "fileOutput", map[string]any{"path": "a.txt"}, textOutput("x")); err == nil || err.Error() != want {
		t.Fatalf("fileOutput error = %v", err)
	}
}

// The hosted API refuses a run by asking this question, so it has to answer it
// from the graph alone.
func TestLocalOnlyNodes(t *testing.T) {
	graph := func(types ...string) *Graph {
		g := &Graph{}
		for i, typ := range types {
			g.Nodes = append(g.Nodes, GraphNode{ID: fmt.Sprintf("n%d", i), Type: typ})
		}
		return g
	}
	cases := []struct {
		name  string
		graph *Graph
		want  []string
	}{
		{"a graph that touches no files runs anywhere",
			graph("textInput", "llm", "generateImage", "preview"), nil},
		{"a reader needs the desktop app",
			graph("fileInput", "vision", "output"), []string{"fileInput"}},
		{"a writer does too",
			graph("textInput", "llm", "fileOutput"), []string{"fileOutput"}},
		{"both, once each and in a stable order, whatever the graph's order",
			graph("fileOutput", "fileInput", "fileOutput", "llm", "fileInput"),
			[]string{"fileInput", "fileOutput"}},
		{"an empty graph needs nothing", graph(), nil},
		{"a nil graph is not a crash", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LocalOnlyNodes(tc.graph)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A fingerprint describes the graph, not the disk. Replaying a file node was a
// real failure: a run whose output file had been deleted reported success and
// recreated nothing, because the write was served from the cache.
func TestFileNodesAreNeverReplayedFromCache(t *testing.T) {
	graph := &Graph{Nodes: []GraphNode{
		{ID: "a", Type: "fileInput", Data: map[string]any{"config": map[string]any{"path": "in.txt"}}},
		{ID: "b", Type: "fileOutput", Data: map[string]any{"config": map[string]any{"path": "out.txt"}}},
		{ID: "c", Type: "llm", Data: map[string]any{"config": map[string]any{}}},
	}}

	// Every node has a matching, completed cache entry, so anything replayable
	// would be replayed here.
	entry := func(id string) CacheEntry {
		return CacheEntry{Fingerprint: NodeFingerprint(id), Output: textOutput("from the cache")}
	}
	rt := &Runtime{
		Graph:        graph,
		Fingerprints: map[string]NodeFingerprint{"a": "a", "b": "b", "c": "c"},
		Cache:        map[string]CacheEntry{"a": entry("a"), "b": entry("b"), "c": entry("c")},
	}

	for _, id := range []string{"a", "b"} {
		if _, ok := rt.cachedOutput(id); ok {
			node := NodeByID(graph, id)
			t.Fatalf("node %q (%s) was served from the cache; file nodes must always run", id, node.Type)
		}
	}
	// The guard has to be narrow: an ordinary node still replays, which is the
	// whole point of the cache.
	if _, ok := rt.cachedOutput("c"); !ok {
		t.Fatal("the llm node was not replayed; the cache bypass is too broad")
	}
}
