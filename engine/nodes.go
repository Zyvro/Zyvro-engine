package engine

import (
	"sort"

	"github.com/Zyvro/Zyvro-engine/plugins"
)

// The node catalogue: what the builder is allowed to put on a canvas.
//
// It used to live in the frontend, as a literal the desktop app carried with
// it. That stopped being possible the moment a project could install packs,
// because half the palette then only exists on the machine running the engine.
// So the whole set is declared by the engine instead, built-ins included, and
// the app asks the engine it is talking to what that engine can run.
//
// Most of it is no longer declared in this file. A node that ships as Lua
// describes itself — its label, its ports and its settings sit in the same file
// as its behaviour, which is the shape a node in a store has to have — and
// Catalogue reads those off the registry. What is left here is the handful the
// engine still runs in Go.

// NodeKind is one entry in the palette. The JSON tags are the frontend's
// NodeKind type field for field (see Zyvro-frontend/src/lib/nodes.ts), so the
// response of GET /api/nodes can be handed to the builder unchanged.
type NodeKind struct {
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	Inputs      []string `json:"inputs"`
	Outputs     []string `json:"outputs"`
	// Defaults is the config a freshly placed node starts with. It is always
	// present, even empty: the builder spreads it into the new node's config,
	// and a missing object there reads as "this node has no settings" rather
	// than "this node has settings nobody told me about".
	Defaults map[string]any `json:"defaults"`
	// ToolOnly marks a node with no data ports, which exists only to be bound
	// to a Brain through a tool edge.
	ToolOnly bool `json:"toolOnly,omitempty"`
	// LocalOnly marks a node that needs a project folder on the machine running
	// the engine. The builder still shows it everywhere: seeing that the
	// capability exists is how someone learns the desktop app is worth having.
	LocalOnly bool `json:"localOnly,omitempty"`
	// Pack names the pack a node came from, and is absent on built-ins. The
	// palette badges it, because someone looking at a node that behaves oddly
	// should be able to see at a glance that it did not come from us.
	Pack string `json:"pack,omitempty"`
}

// builtinKinds is the palette entry for every node the engine still implements
// in Go: the input nodes that read the run's inputs, the file nodes that reach
// the project folder, and the two that hand their value straight on.
//
// Everything else the engine ships is a Lua file in the bundled pack and
// describes itself — its label, its ports and its settings are in the same file
// as its behaviour, which is the shape a node in a store has to have. This list
// is what is left, and it is deliberately not derived from the switch in
// executeWithInput: a switch case knows how to run a node, not what to call it,
// and generating one from the other would only mean the two could never
// disagree out loud. They are checked against each other in a test instead.
var builtinKinds = []NodeKind{
	{
		Type:        "textInput",
		Label:       "Text Input",
		Category:    "Input",
		Description: "Static text or runtime input",
		Inputs:      []string{},
		Outputs:     []string{"text"},
		Defaults:    map[string]any{"value": "", "inputKey": ""},
	},
	{
		Type:        "imageInput",
		Label:       "Image Input",
		Category:    "Input",
		Description: "Uploaded image or runtime input",
		Inputs:      []string{},
		Outputs:     []string{"image"},
		Defaults:    map[string]any{"dataUrl": "", "inputKey": ""},
	},
	{
		Type:        "fileInput",
		Label:       "Read File",
		Category:    "Input",
		Description: "Read a file from the project folder (desktop only)",
		Inputs:      []string{},
		Outputs:     []string{"any"},
		Defaults:    map[string]any{"path": "", "as": "auto"},
		LocalOnly:   true,
	},
	{
		Type:        "mergeText",
		Label:       "Merge Text",
		Category:    "Utility",
		Description: "Concatenate upstream texts",
		Inputs:      []string{"any", "any"},
		Outputs:     []string{"text"},
		// The separator default is the two characters backslash-n, not a
		// newline: runMergeText unescapes it, so what the form shows is what a
		// person can edit without a text area that eats their line breaks.
		Defaults: map[string]any{"separator": "\\n"},
	},
	{
		Type:        "preview",
		Label:       "Preview",
		Category:    "Output",
		Description: "Render a result in the node",
		Inputs:      []string{"any"},
		Outputs:     []string{},
		Defaults:    map[string]any{},
	},
	{
		Type:        "fileOutput",
		Label:       "Write File",
		Category:    "Output",
		Description: "Write the result into the project folder (desktop only)",
		// The node hands its input straight back out, so a chain can keep going
		// after writing: read, transform, write, then preview what was written.
		Inputs:    []string{"any"},
		Outputs:   []string{"any"},
		Defaults:  map[string]any{"path": "", "createDirs": true},
		LocalOnly: true,
	},
	{
		Type:        "output",
		Label:       "Output",
		Category:    "Output",
		Description: "Mark a workflow result",
		Inputs:      []string{"any"},
		Outputs:     []string{},
		Defaults:    map[string]any{},
	},
}

// disabledNodeTypes are node types executeWithInput still has a case for, but
// which refuse to run. They are deliberately absent from the palette: a node
// nobody can place is better than one that can be placed and then fails, and
// the case stays so that an old workflow still gets the real explanation rather
// than "unknown node type".
//
// It is empty, and has been since generateVideo left it. The list stays because
// the situation it describes will happen again — a node whose backend is gone,
// or not there yet — and because the palette and the executor are checked
// against each other by a test that needs somewhere to read the exceptions.
var disabledNodeTypes = []string{}

// BuiltinKinds returns the palette entries for the engine's own nodes. The
// copies are deep, because a caller renders this and must not be able to reach
// back into the table every later caller will be served from.
func BuiltinKinds() []NodeKind {
	out := make([]NodeKind, len(builtinKinds))
	for i, k := range builtinKinds {
		out[i] = k.clone()
	}
	return out
}

// Catalogue is the whole palette: the built-ins, then every node the installed
// packs contribute, in a stable order. A nil registry is a host with no packs.
//
// Built-ins and pack nodes come back in one list rather than two because that
// is what the thing asking is: "what can I place on a canvas". The builder
// drops anything a pack contributed under a built-in name, and so does the
// registry, so a collision cannot reach here in the first place.
func Catalogue(reg *plugins.Registry) []NodeKind {
	out := BuiltinKinds()
	if reg == nil {
		// A caller with no registry still gets the whole palette: most of it
		// now lives in the bundled pack, and a host that never heard of packs
		// must not end up offering seven nodes.
		reg = defaultRegistry()
	}
	for _, def := range reg.Kinds() {
		out = append(out, KindFromPluginNode(def))
	}
	return out
}

// KindFromPluginNode turns a pack's node definition into a palette entry.
func KindFromPluginNode(def plugins.NodeDef) NodeKind {
	k := NodeKind{
		Type:        def.Type,
		Label:       def.Label,
		Category:    def.Category,
		Description: def.Description,
		Inputs:      append([]string{}, def.Inputs...),
		Outputs:     append([]string{}, def.Outputs...),
		Defaults:    map[string]any{},
		ToolOnly:    def.ToolOnly,
		Pack:        def.Pack,
	}
	// The bundled pack is not a pack as far as anybody looking at the palette
	// is concerned: the badge exists so that a node behaving oddly can be seen
	// at a glance not to have come from us, and these did.
	if k.Pack == BuiltinPackName {
		k.Pack = ""
	}
	// A pack describes its settings as form fields; the palette wants the
	// starting config those fields would produce. A field with no default still
	// gets a key, because a config that grows a key the first time someone
	// touches a control is a config whose fingerprint changes for no reason.
	for _, f := range def.Config {
		k.Defaults[f.Key] = defaultForField(f)
	}
	return k
}

// defaultForField is what a config field starts at when the pack named nothing.
func defaultForField(f plugins.ConfigField) any {
	if f.Default != nil {
		return f.Default
	}
	switch f.Type {
	case "number":
		return 0
	case "boolean":
		return false
	case "select":
		// The empty string is not one of the options, and a select showing a
		// value it cannot offer is a control that looks broken.
		if len(f.Options) > 0 {
			return f.Options[0]
		}
		return ""
	default:
		return ""
	}
}

// BuiltinNodeTypes returns the node types the engine implements in Go, sorted.
// It is the palette above plus the disabled cases, and it is what a pack may
// not take: it is handed to plugins.NewRegistry and to plugins.Load, which is
// the whole of how that package learns what the names are.
//
// The bundled pack's own types are not in it, and do not need to be. That pack
// is installed into the registry first, so a later pack claiming one of its
// names collides with it and is refused by name.
func BuiltinNodeTypes() []string {
	out := make([]string, 0, len(builtinKinds)+len(disabledNodeTypes))
	for _, k := range builtinKinds {
		out = append(out, k.Type)
	}
	out = append(out, disabledNodeTypes...)
	sort.Strings(out)
	return out
}

// RunnableBuiltinNodeTypes is every node this engine actually offers: the Go
// switch cases and the bundled pack's Lua nodes, minus the ones that are
// disabled. It is what a palette should contain, which is what separates it
// from ReservedNodeTypes — a disabled type is still a name a pack may not take,
// and still not a node anyone can use.
//
// Installed packs are deliberately absent: they are a property of a project,
// not of this binary.
func RunnableBuiltinNodeTypes() []string {
	disabled := map[string]bool{}
	for _, t := range disabledNodeTypes {
		disabled[t] = true
	}
	out := make([]string, 0, len(builtinKinds))
	for _, t := range ReservedNodeTypes() {
		if !disabled[t] {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// ReservedNodeTypes is every name a pack may not take: the ones above, and the
// ones the bundled pack defines.
//
// It is one list because there is one question. A pack calling itself "llm"
// and a pack calling itself "textInput" are the same attempt — every workflow
// already using that node would start running the pack's code — and the fact
// that one of those names is a Go switch case and the other a Lua file in the
// binary is an implementation detail the person installing the pack should
// never have to know. Both are refused, at load, by name.
func ReservedNodeTypes() []string {
	out := BuiltinNodeTypes()
	if pack, err := BuiltinPack(); err == nil {
		for _, def := range pack.Nodes {
			out = append(out, def.Type)
		}
	}
	sort.Strings(out)
	return out
}

func (k NodeKind) clone() NodeKind {
	out := k
	out.Inputs = append([]string{}, k.Inputs...)
	out.Outputs = append([]string{}, k.Outputs...)
	out.Defaults = make(map[string]any, len(k.Defaults))
	for key, v := range k.Defaults {
		out.Defaults[key] = v
	}
	return out
}
