package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Zyvro/Zyvro-engine/providers"
)

// FileAccess is the engine's window onto a project folder. It is nil on the
// hosted server, which is what makes the file nodes impossible there rather
// than merely disabled.
type FileAccess interface {
	// Read returns the file's bytes and its media type. rel is relative to the
	// project root; an implementation must reject anything that escapes it.
	Read(rel string) (data []byte, mime string, err error)
	// Write creates or replaces a file and returns the path it actually wrote,
	// relative to the project root.
	Write(rel string, data []byte) (written string, err error)
}

// DirCreatingWriter is the optional half of FileAccess. Write alone cannot say
// whether missing parent directories may be created, so an implementation that
// wants to honour the fileOutput node's "createDirs" setting implements this as
// well; against a plain FileAccess the node can only call Write and the setting
// has no way to reach the filesystem.
type DirCreatingWriter interface {
	WriteWithDirs(rel string, data []byte, createDirs bool) (written string, err error)
}

// maxFileInputBytes caps what one file node may carry. Everything an input
// produces travels through the graph as a base64 data URL held in memory, so a
// 200 MB video would not be a slow node, it would be the end of the process.
const maxFileInputBytes = 25 << 20

// localOnlyMessage is what both file nodes say when the engine has no project
// folder. It names the product rather than the missing dependency because the
// person reading it is a workflow author, not an operator.
const localOnlyMessage = "this node reads and writes files in your project folder, so it only runs in Zyvro Studio, the desktop app"

// outsideTheFingerprint reports whether a node's result depends on, or changes,
// something the fingerprint does not describe. Such a node is never replayed
// from the cache, because a matching fingerprint would not mean a matching
// result.
//
// Two kinds of node qualify. The file nodes read and write the user's project,
// and neither side of that is hashed: replaying a write skips the write, so
// deleting the output and running again would silently produce nothing, and
// replaying a read returns the contents the file had last time even after it
// changed on disk. An installed pack's node is the same problem one step
// further out — its behaviour is Lua in a folder the user can edit, and not one
// byte of that script is in the fingerprint, so editing a node's code and
// running again would replay the old answer forever. Both are wrong in the same
// way, which is why they are one question here rather than two checks at the
// call site.
//
// The bundled pack is the exception, and it has to be: nearly every node worth
// caching now lives in it, and an llm node that could not be replayed would
// mean the cache had quietly stopped existing. Its Lua is compiled into the
// binary, so it changes when the binary changes and never between two runs of
// the same one — which is exactly the property the Go switch had.
func (r *Runtime) outsideTheFingerprint(nodeType string) bool {
	for _, t := range LocalOnlyNodeTypes {
		if t == nodeType {
			return true
		}
	}
	if reg := r.registry(); reg != nil {
		if def, ok := reg.Kind(nodeType); ok {
			// A pack node used to be excluded wholesale, because its behaviour
			// is a script the user can edit and not one byte of it was in the
			// fingerprint. That byte is in it now, so the only question left is
			// the one that was always the real one: does this node reach
			// outside the graph? The `files` capability is what lets it, and it
			// is exactly what the two file nodes above have.
			return def.Has("files")
		}
	}
	return false
}

// LocalOnlyNodeTypes are the node types that need a project folder on the
// machine running the engine.
var LocalOnlyNodeTypes = []string{"fileInput", "fileOutput"}

// LocalOnlyNodes reports which of a graph's nodes need a local engine, so a
// hosted caller can refuse the run with an explanation instead of failing
// halfway through it.
func LocalOnlyNodes(g *Graph) []string {
	if g == nil {
		return nil
	}
	present := map[string]bool{}
	for _, n := range g.Nodes {
		present[n.Type] = true
	}
	// Ordered by LocalOnlyNodeTypes rather than by where the nodes happen to
	// sit in the graph, so the same graph always produces the same list and a
	// caller can compare or display it without sorting first.
	var out []string
	for _, typ := range LocalOnlyNodeTypes {
		if present[typ] {
			out = append(out, typ)
		}
	}
	return out
}

func errLocalOnly() error { return fmt.Errorf("%s", localOnlyMessage) }

// ---------- file input ----------

// runFileInput reads one file from the project folder and turns it into the
// same kind of output a textInput, imageInput or json-producing node would
// give, so the rest of the graph cannot tell where the value came from.
func (r *Runtime) runFileInput(in *RunInput) (*NodeOutput, error) {
	if r.Files == nil {
		return nil, errLocalOnly()
	}
	// The path goes through the input resolver first: a workflow that is run
	// over a list of files drives this node with {{input:...}} rather than
	// being edited between runs.
	resolved, err := r.resolveInputs(str(in.Config["path"]))
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(resolved)
	if path == "" {
		return nil, fmt.Errorf("file input needs a path relative to the project folder")
	}
	data, mimeType, err := r.Files.Read(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if len(data) > maxFileInputBytes {
		// A *providers.ProviderError, like a provider refusing an oversized
		// request, because this is the same class of refusal: the file is fine,
		// it just cannot travel through a node output.
		return nil, &providers.ProviderError{
			Code: "file_too_large",
			Message: fmt.Sprintf("%s is %s, over the %s limit for a file node",
				path, humanBytes(len(data)), humanBytes(maxFileInputBytes)),
		}
	}
	mimeType = cleanMime(mimeType)

	switch as := strings.ToLower(strDefault(in.Config["as"], "auto")); as {
	case "auto":
		return fileInputValue(autoKind(path, mimeType), path, mimeType, data)
	case "text", "image", "json":
		return fileInputValue(as, path, mimeType, data)
	default:
		return nil, fmt.Errorf("file input: unknown mode %q (use auto, text, image or json)", as)
	}
}

// fileInputValue turns raw bytes into the node output of one kind, failing
// loudly when the bytes are not what that kind needs.
func fileInputValue(kind, path, mimeType string, data []byte) (*NodeOutput, error) {
	switch kind {
	case "image":
		if !strings.HasPrefix(mimeType, "image/") {
			// The store's guess can be missing or wrong (an extensionless
			// file), so the bytes get the last word before we refuse.
			if sniffed := cleanMime(http.DetectContentType(data)); strings.HasPrefix(sniffed, "image/") {
				mimeType = sniffed
			} else {
				return nil, fmt.Errorf("%s is not an image (%s)", path, orUnknown(mimeType))
			}
		}
		// Deliberately the same constructor imageInput uses: vision, editImage
		// and removeBackground read dataUrl out of this map, so the shape is a
		// contract between nodes, not a detail of this one.
		return imageOutput(mimeType, data), nil
	case "json":
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
		return jsonOutput(v), nil
	default:
		text, err := decodeText(data)
		if err != nil {
			return nil, fmt.Errorf("%s %w", path, err)
		}
		return textOutput(text), nil
	}
}

// autoKind decides what a file is from its extension, with the store's media
// type as a second opinion for files whose extension says nothing.
func autoKind(path, mimeType string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff", ".avif", ".heic":
		return "image"
	case ".json":
		return "json"
	}
	if strings.HasPrefix(mimeType, "image/") {
		return "image"
	}
	if mimeType == "application/json" {
		return "json"
	}
	return "text"
}

// decodeText refuses binary rather than handing the graph a string full of
// replacement characters: a prompt built from those is worse than a failed run,
// because it looks like it worked.
func decodeText(data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", fmt.Errorf("is not valid UTF-8 text; read it with as: \"image\" or point the node at a text file")
	}
	// A NUL byte is valid UTF-8 and never appears in a text file anyone meant
	// to read, so it is the cheapest reliable sign of a binary file that
	// happens to decode.
	if bytesContainNUL(data) {
		return "", fmt.Errorf("looks like a binary file, not text; read it with as: \"image\" or point the node at a text file")
	}
	return string(data), nil
}

func bytesContainNUL(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

// ---------- file output ----------

// runFileOutput writes whatever reaches it to the project folder.
func (r *Runtime) runFileOutput(in *RunInput) (*NodeOutput, error) {
	if r.Files == nil {
		return nil, errLocalOnly()
	}
	var src *NodeOutput
	for _, u := range in.Upstream {
		if u != nil {
			src = u
			break
		}
	}
	if src == nil {
		return nil, fmt.Errorf("file output node has nothing to write: connect the node whose result should be saved")
	}
	resolvedPath, err := r.resolveInputs(str(in.Config["path"]))
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(resolvedPath)
	if path == "" {
		return nil, fmt.Errorf("file output needs a path relative to the project folder")
	}
	data, err := outputBytes(src)
	if err != nil {
		return nil, err
	}

	createDirs := boolDefault(in.Config["createDirs"], true)
	var written string
	if w, ok := r.Files.(DirCreatingWriter); ok {
		written, err = w.WriteWithDirs(path, data, createDirs)
	} else {
		written, err = r.Files.Write(path, data)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", path, err)
	}

	// The input passes through unchanged apart from savedPath, so the node
	// still previews what it wrote and anything downstream of it keeps working
	// on the value: saving a file is a side effect of the chain, not its end.
	value := make(map[string]any, len(src.Value)+1)
	for k, v := range src.Value {
		value[k] = v
	}
	value["savedPath"] = written
	return &NodeOutput{Type: src.Type, Value: value}, nil
}

// outputBytes is the on-disk form of a node output: the bytes a person would
// expect to find in the file, not the JSON envelope the engine moves around.
func outputBytes(out *NodeOutput) ([]byte, error) {
	switch out.Type {
	case "image":
		_, data, err := parseDataURL(str(out.Value["dataUrl"]))
		if err != nil {
			return nil, fmt.Errorf("file output: image input invalid: %w", err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("file output: image input carries no image")
		}
		return data, nil
	case "text":
		return []byte(str(out.Value["text"])), nil
	default:
		// json, boolean, number and anything added later: the payload lives
		// under "data" when there is one, and indented JSON with a trailing
		// newline is what a person opening the file in an editor expects.
		var v any = out.Value
		if d, ok := out.Value["data"]; ok {
			v = d
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("file output: cannot encode %s output as JSON: %w", out.Type, err)
		}
		return append(b, '\n'), nil
	}
}

// ---------- small helpers ----------

// boolDefault reads a config flag that may arrive as a real bool from JSON or
// as a string from a form field.
func boolDefault(v any, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}

// cleanMime drops the parameters of a media type ("; charset=utf-8"), which
// would otherwise end up inside a data URL.
func cleanMime(m string) string {
	if i := strings.Index(m, ";"); i >= 0 {
		m = m[:i]
	}
	return strings.ToLower(strings.TrimSpace(m))
}

func orUnknown(m string) string {
	if m == "" {
		return "unknown type"
	}
	return m
}

func humanBytes(n int) string {
	const mb = 1 << 20
	if n >= mb {
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}
