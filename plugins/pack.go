package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// A pack is a directory:
//
//	<pack>/
//	  zyvro-pack.json
//	  nodes/*.lua
//	  workflows/*.json   optional examples
//
// JSON rather than TOML for the manifest, because JSON needs no dependency and
// this package is allowed exactly one.
const (
	manifestName    = "zyvro-pack.json"
	altManifestName = "zyvro-pack.toml"
	nodesDir        = "nodes"
	// maxNodeSourceBytes caps a single node file. A node is a few hundred lines
	// of Lua; a megabyte of it is either a mistake or an attempt to make the
	// parser the expensive part.
	maxNodeSourceBytes = 1 << 20
	// maxNodesPerPack caps how many node files one pack may contribute, so a
	// directory of ten thousand tiny chunks cannot make installation the attack.
	maxNodesPerPack = 128
)

var (
	packNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	nodeTypeRE  = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]{0,63}$`)
	versionRE   = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,31}$`)
	configKeyRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)
)

// Capabilities a pack may declare. The list is closed, and an unknown entry is
// an error rather than something ignored: a manifest asking for "network" must
// fail loudly today, so that adding that capability tomorrow cannot silently
// grant it to a pack installed before it existed.
//
// They are separate rather than one "ai" capability because they are separate
// decisions. Someone installing a pack is being asked to agree to something
// specific, and "this pack may describe images to a model" and "this pack may
// run an agent loop against your account" are not the same sentence. A pack
// that only needs one must not have to ask for both.
const (
	// CapLLM is one text completion at a time, through the host's own provider.
	CapLLM = "llm"
	// CapFiles is the project folder the user already opened.
	CapFiles = "files"
	// CapImage is the host making an image on the node's behalf: generating
	// one, editing one, removing a background, turning or mirroring one, or
	// composing several into a preview. None of that work happens in Lua — the
	// capability is permission to ask the host to do it.
	CapImage = "image"
	// CapVision is sending images to a model and getting words back.
	CapVision = "vision"
	// CapAgent is the Brain: a loop that calls the model repeatedly and
	// executes other nodes of the graph as tools. It is the widest of the five,
	// because a Brain reaches nodes this one is not.
	CapAgent = "agent"
	// CapVideo is the host rendering a video on the node's behalf. It is not
	// part of CapImage, and the separation is the point of this list: a video
	// is billed by the second and takes a minute, where an image is billed once
	// and takes a moment. "This pack may make you an image" and "this pack may
	// spend a minute of video per call" are not the same sentence.
	CapVideo = "video"
)

var knownCapabilities = []string{CapLLM, CapFiles, CapImage, CapVision, CapAgent, CapVideo}

// A note on reserved node types, which used to live here as a list.
//
// It was a copy of the engine's switch statement, kept in step by a test that
// read both files as Go syntax, and it existed because plugins must not import
// engine. That is still true, so the names still have to arrive from outside —
// but a list is not the only way to be told. They are now passed in: Load takes
// them, NewRegistry takes them, and the engine, which is the only thing that
// knows what it can run, is the one that says. One list, in the package that
// owns it.
//
// Nothing is lost by the inversion. A name a pack may not take is still refused
// twice, at load and again at install, for the same reason as before: a
// built-in added after a pack shipped would otherwise be shadowed by a pack
// that loaded cleanly last week, and shadowing "llm" would be a very good
// attack.

// portTypes are the values a node's inputs and outputs may name. They mirror
// the engine's NodeOutput.Type, plus "any" for a node that does not care.
var portTypes = []string{"text", "image", "video", "json", "any"}

// categories are the palette sections a node may be filed under.
var categories = []string{"Input", "AI", "Utility", "Output", "Agent"}

// configTypes are the form controls a node may ask the builder to render.
var configTypes = []string{"text", "textarea", "number", "boolean", "select"}

// Manifest is what zyvro-pack.json holds.
type Manifest struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Author       string   `json:"author"`
	Capabilities []string `json:"capabilities"`
}

// ConfigField is one control in a node's configuration form.
type ConfigField struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Type    string   `json:"type"`
	Default any      `json:"default,omitempty"`
	Options []string `json:"options,omitempty"`
}

// NodeDef is one node type a pack contributes: everything the builder needs to
// draw it, plus the compiled chunk that produces its behaviour.
type NodeDef struct {
	Type        string        `json:"type"`
	Label       string        `json:"label"`
	Category    string        `json:"category"`
	Description string        `json:"description"`
	Inputs      []string      `json:"inputs"`
	Outputs     []string      `json:"outputs"`
	Config      []ConfigField `json:"config"`

	// ToolOnly marks a node with no data ports at all, which exists only to be
	// bound to a Brain through a tool edge. The palette draws it without
	// handles, and the DAG loop never reaches it.
	ToolOnly bool `json:"toolOnly,omitempty"`

	// Pack and Capabilities travel with the definition because they decide what
	// its ctx is allowed to carry. A node cannot be run without knowing which
	// pack declared it.
	Pack         string   `json:"pack"`
	Capabilities []string `json:"capabilities"`

	// Source is the file the definition came from, for error messages.
	Source string `json:"source"`

	// CodeDigest is a hash of the exact bytes that were compiled for this
	// node. It exists so a caching host can tell two versions of a node
	// apart: a node's behaviour is its script, and a script in a folder the
	// user can edit changes between two runs of the same binary. Without it
	// the only safe answer was to never replay a pack node at all.
	CodeDigest string `json:"codeDigest"`

	// proto is the compiled chunk, not the run function. A Lua closure belongs
	// to the state it was made in, and every run gets a fresh state, so what is
	// kept between runs is the compiled code rather than anything live. That is
	// also what makes runs independent: nothing a script writes to its globals
	// can outlive it.
	proto *lua.FunctionProto
}

// clone returns a copy safe to hand to a caller, with its slices detached so
// nothing outside this package can reach into a registered definition.
func (d NodeDef) clone() NodeDef {
	out := d
	out.Inputs = append([]string(nil), d.Inputs...)
	out.Outputs = append([]string(nil), d.Outputs...)
	out.Capabilities = append([]string(nil), d.Capabilities...)
	out.Config = make([]ConfigField, len(d.Config))
	for i, f := range d.Config {
		f.Options = append([]string(nil), d.Config[i].Options...)
		out.Config[i] = f
	}
	return out
}

// Has reports whether the pack this node came from declared a capability.
func (d *NodeDef) Has(capability string) bool {
	for _, c := range d.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Pack is a loaded, validated pack.
type Pack struct {
	Dir      string
	Manifest Manifest
	Nodes    []*NodeDef
}

// Load reads a pack directory, validates its manifest, and evaluates each node
// file to collect its definition.
//
// reserved are the node types the host already implements and a pack therefore
// may not define. They are a parameter rather than a list in this file because
// this package cannot import the one that knows them; a nil slice reserves
// nothing, which is what the host's own bundled pack is loaded with.
//
// The evaluation happens in the same sandbox a run gets, with a shorter
// deadline. That is the point rather than an economy: returning a table is not
// work, so a chunk that needs a looser sandbox to produce its definition is
// doing something at load time that it should not be able to do at all.
func Load(dir string, reserved []string) (*Pack, error) {
	// os.DirFS rather than the paths directly, so that this and LoadFS are one
	// implementation. A pack read from an embedded filesystem has to be loaded
	// by exactly the code that loads one from disk, or the bundled pack would
	// be validated by something other than the validator.
	return loadFS(os.DirFS(dir), ".", dir, reserved)
}

// LoadFS is Load against any filesystem, which is how a pack compiled into the
// binary is read. root is the directory inside fsys that holds the manifest.
func LoadFS(fsys fs.FS, root string, reserved []string) (*Pack, error) {
	if root == "" {
		root = "."
	}
	return loadFS(fsys, root, root, reserved)
}

// loadFS is the whole of both. label is what error messages call the pack's
// location, which for a directory on disk is the path the user typed and for an
// embedded pack is the name it has inside the binary.
func loadFS(fsys fs.FS, root, label string, reserved []string) (*Pack, error) {
	manifest, err := readManifest(fsys, root, label)
	if err != nil {
		return nil, err
	}

	files, err := nodeFiles(fsys, root, label)
	if err != nil {
		return nil, err
	}

	pack := &Pack{Dir: label, Manifest: *manifest}
	seen := map[string]string{}
	for _, file := range files {
		def, err := loadNodeFile(fsys, file, manifest, reserved)
		if err != nil {
			return nil, err
		}
		if other, dup := seen[def.Type]; dup {
			return nil, fmt.Errorf("%s: node type %q is already defined by %s",
				path.Base(file), def.Type, path.Base(other))
		}
		seen[def.Type] = file
		pack.Nodes = append(pack.Nodes, def)
	}
	if len(pack.Nodes) == 0 {
		return nil, fmt.Errorf("pack %s has no nodes: put at least one .lua file in %s/", manifest.Name, nodesDir)
	}
	return pack, nil
}

func readManifest(fsys fs.FS, root, label string) (*Manifest, error) {
	data, err := fs.ReadFile(fsys, path.Join(root, manifestName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, altErr := fs.Stat(fsys, path.Join(root, altManifestName)); altErr == nil {
				return nil, fmt.Errorf("%s: this pack has a %s, but Zyvro reads %s", label, altManifestName, manifestName)
			}
			return nil, fmt.Errorf("%s: no %s in this directory", label, manifestName)
		}
		return nil, err
	}

	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(data)))
	// Unknown fields are refused, because a manifest field nobody reads is a
	// field whose meaning the author assumed. Better to say it is not a thing.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", manifestName, err)
	}

	if !packNameRE.MatchString(m.Name) {
		return nil, fmt.Errorf("%s: name %q is not valid: use lowercase letters, digits and dashes, starting with a letter or digit, up to 64 characters",
			manifestName, m.Name)
	}
	if !versionRE.MatchString(m.Version) {
		return nil, fmt.Errorf("%s: version %q is not valid: use something like \"1.0.0\"", manifestName, m.Version)
	}
	for _, c := range m.Capabilities {
		if !contains(knownCapabilities, c) {
			return nil, fmt.Errorf("%s: unknown capability %q: a pack may declare %s",
				manifestName, c, strings.Join(knownCapabilities, ", "))
		}
	}
	return &m, nil
}

func nodeFiles(fsys fs.FS, root, label string) ([]string, error) {
	dir := path.Join(root, nodesDir)
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: no %s/ directory", label, nodesDir)
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		// Only regular .lua files: a directory named foo.lua, or a symlink out
		// of the pack, is not a node file.
		if e.IsDir() || !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		files = append(files, path.Join(dir, e.Name()))
	}
	if len(files) > maxNodesPerPack {
		return nil, fmt.Errorf("%s: %d node files, over the limit of %d for one pack", label, len(files), maxNodesPerPack)
	}
	// Sorted, so a pack always loads in the same order and an error in it is
	// always reported against the same file.
	sort.Strings(files)
	return files, nil
}

func loadNodeFile(fsys fs.FS, file string, m *Manifest, reserved []string) (*NodeDef, error) {
	name := path.Base(file)
	info, err := fs.Stat(fsys, file)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxNodeSourceBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the limit of %d for one node file", name, info.Size(), maxNodeSourceBytes)
	}
	src, err := fs.ReadFile(fsys, file)
	if err != nil {
		return nil, err
	}
	proto, err := compileChunk(name, src)
	if err != nil {
		return nil, err
	}

	def, _, err := sandboxRun(context.Background(), loadLimits(), func(L *lua.LState) (*NodeDef, error) {
		return evalDefinition(L, name, proto, reserved)
	})
	if err != nil {
		return nil, err
	}

	def.Pack = m.Name
	def.Capabilities = append([]string(nil), m.Capabilities...)
	def.Source = name
	// Hashed from the source rather than from the compiled chunk: the bytes
	// are what the author edited and what a reviewer read, and a compiler that
	// changed its output between versions would otherwise invalidate every
	// cache entry for nodes nobody touched.
	sum := sha256.Sum256(src)
	def.CodeDigest = hex.EncodeToString(sum[:])
	def.proto = proto
	return def, nil
}

// evalDefinition runs a node chunk and reads the table it returns.
func evalDefinition(L *lua.LState, name string, proto *lua.FunctionProto, reserved []string) (*NodeDef, error) {
	// Not wrapped with the file name: a Lua runtime error already carries the
	// chunk name and the line, and prefixing it again reads as two files.
	if err := L.CallByParam(lua.P{Fn: L.NewFunctionFromProto(proto), NRet: 1, Protect: true}); err != nil {
		return nil, err
	}
	ret := L.Get(-1)
	L.Pop(1)
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("%s must end with `return { ... }` describing the node, but returned %s", name, ret.Type().String())
	}
	def, err := readDefinition(tbl, reserved)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return def, nil
}

func readDefinition(tbl *lua.LTable, reserved []string) (*NodeDef, error) {
	def := &NodeDef{
		Type:        luaString(tbl, "type"),
		Label:       luaString(tbl, "label"),
		Category:    luaString(tbl, "category"),
		Description: luaString(tbl, "description"),
	}

	if !nodeTypeRE.MatchString(def.Type) {
		return nil, fmt.Errorf("type %q is not valid: use a letter followed by letters, digits or underscores, up to 64 characters", def.Type)
	}
	// By name, before anything else is checked. A pack whose node is called
	// "llm" is not a pack with a naming problem.
	if contains(reserved, def.Type) {
		return nil, fmt.Errorf("type %q is a built-in Zyvro node and cannot be redefined by a pack", def.Type)
	}
	if def.Label == "" {
		def.Label = def.Type
	}
	if def.Category == "" {
		def.Category = "Utility"
	}
	if !contains(categories, def.Category) {
		return nil, fmt.Errorf("category %q is not valid: use one of %s", def.Category, strings.Join(categories, ", "))
	}

	var err error
	if def.Inputs, err = readPorts(tbl, "inputs"); err != nil {
		return nil, err
	}
	if def.Outputs, err = readPorts(tbl, "outputs"); err != nil {
		return nil, err
	}
	def.ToolOnly = luaBool(tbl, "toolOnly")
	// A node with no outputs used to be refused, on the grounds that nobody
	// could do anything with its result. Two kinds of node disprove that: a
	// terminal one, which renders what reaches it and hands nothing on, and a
	// tool-only one, which the Brain calls and the data flow never touches. So
	// the rule is now narrower — a node has to have somewhere for its value to
	// go, or say that it has not on purpose.
	if len(def.Outputs) == 0 && !def.ToolOnly && len(def.Inputs) == 0 {
		return nil, fmt.Errorf("this node has no inputs and no outputs: name an output (one of %s), or set toolOnly = true if it exists only to be bound to a Brain",
			strings.Join(portTypes, ", "))
	}
	if def.Config, err = readConfig(tbl); err != nil {
		return nil, err
	}
	if _, ok := tbl.RawGetString("run").(*lua.LFunction); !ok {
		return nil, fmt.Errorf("run must be a function taking the node's ctx")
	}
	return def, nil
}

func readPorts(tbl *lua.LTable, key string) ([]string, error) {
	list, err := luaStringList(tbl, key)
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		if !contains(portTypes, p) {
			return nil, fmt.Errorf("%s: %q is not a port type: use one of %s", key, p, strings.Join(portTypes, ", "))
		}
	}
	return list, nil
}

func readConfig(tbl *lua.LTable) ([]ConfigField, error) {
	v := tbl.RawGetString("config")
	if v == lua.LNil {
		return nil, nil
	}
	list, ok := v.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("config must be a list of fields")
	}
	seen := map[string]bool{}
	out := make([]ConfigField, 0, list.Len())
	for i := 1; i <= list.Len(); i++ {
		entry, ok := list.RawGetInt(i).(*lua.LTable)
		if !ok {
			return nil, fmt.Errorf("config[%d] must be a table like { key = \"…\", label = \"…\", type = \"text\" }", i)
		}
		f := ConfigField{
			Key:   luaString(entry, "key"),
			Label: luaString(entry, "label"),
			Type:  luaString(entry, "type"),
		}
		if !configKeyRE.MatchString(f.Key) {
			return nil, fmt.Errorf("config[%d]: key %q is not valid: use a letter or underscore followed by letters, digits or underscores", i, f.Key)
		}
		if seen[f.Key] {
			return nil, fmt.Errorf("config[%d]: key %q appears twice", i, f.Key)
		}
		seen[f.Key] = true
		if f.Label == "" {
			f.Label = f.Key
		}
		if f.Type == "" {
			f.Type = "text"
		}
		if !contains(configTypes, f.Type) {
			return nil, fmt.Errorf("config[%d] (%s): type %q is not valid: use one of %s", i, f.Key, f.Type, strings.Join(configTypes, ", "))
		}
		options, err := luaStringList(entry, "options")
		if err != nil {
			return nil, fmt.Errorf("config[%d] (%s): %w", i, f.Key, err)
		}
		f.Options = options
		if f.Type == "select" && len(f.Options) == 0 {
			return nil, fmt.Errorf("config[%d] (%s): a select field needs an options list", i, f.Key)
		}
		budget := maxValueNodes
		def, err := fromLua(entry.RawGetString("default"), 0, &budget)
		if err != nil {
			return nil, fmt.Errorf("config[%d] (%s): default: %w", i, f.Key, err)
		}
		f.Default = def
		out = append(out, f)
	}
	return out, nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
