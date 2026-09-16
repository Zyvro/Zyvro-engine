package plugins

import (
	"context"
	"fmt"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// Everything a Lua node can reach is in this file. The sandbox decides what a
// script cannot do; this decides what it can, and the two together are the
// whole of a pack's authority. Nothing is exposed here that the node could not
// have been given as data, except the two things a node is for: one model call
// and, when the pack asked for it, the project folder the user already opened.

// Output mirrors engine.NodeOutput. It is mirrored rather than imported because
// engine imports this package; keeping the shapes in step by hand is the cost
// of not having a cycle.
type Output struct {
	// Type is a port type: text, image or json.
	Type string `json:"type"`
	// Value carries the payload the way the engine's own nodes do — text under
	// "text", an image as a data URL under "dataUrl" — so nothing downstream
	// can tell a pack node from a built-in one.
	Value map[string]any `json:"value"`
}

// LLMRequest is one model call from inside a node.
type LLMRequest struct {
	Prompt    string
	System    string
	Model     string
	MaxTokens int
}

// LLMFunc is how the host lends a node the user's model account. The host
// supplies it, which is what keeps provider selection, credentials and routing
// on the Go side of the boundary: a pack names a model, it never names an
// endpoint or a key.
type LLMFunc func(ctx context.Context, req LLMRequest) (string, error)

// FileAccess is the same interface engine.FileAccess declares, so a host can
// pass the one it already holds.
//
// There is deliberately no second path validator in this package. The one
// behind this interface resolves symlinks, refuses anything that leaves the
// project root and refuses .zyvro; writing another would mean two
// implementations of the same rule, and the weaker one would be the one that
// mattered.
type FileAccess interface {
	Read(rel string) (data []byte, mime string, err error)
	Write(rel string, data []byte) (written string, err error)
}

// HostInput is everything one node execution is given.
type HostInput struct {
	// Input is the upstream node's value, or nil when nothing is connected.
	Input *Output
	// Config is the node's configuration, already resolved by the host — the
	// {{input:...}} placeholders are substituted before they get here, because
	// resolving them is the runtime's job and a script must not see the
	// unresolved form.
	Config map[string]any
	// LLM and Files are the two capabilities. A nil one is a capability the
	// host is not granting, and the matching function is then absent from ctx
	// rather than present and failing.
	LLM   LLMFunc
	Files FileAccess
	// Limits overrides DefaultLimits for this execution. Zero fields fall back.
	Limits Limits
}

// maxDataURLBytes caps an image a node may return. It is larger than
// MaxStringBytes because base64 of a legitimate image is genuinely big, and
// smaller than the file gate's 25 MB because this value travels through the
// whole graph in memory.
const maxDataURLBytes = 12 << 20

// host is the Go side of one node execution. Its methods run only on the
// goroutine sandboxRun owns, so the counters need no locking.
type host struct {
	ctx      context.Context
	def      *NodeDef
	in       HostInput
	limits   Limits
	log      *runLog
	llmCalls int
}

// runNode executes one node definition and returns its output and its log.
func runNode(ctx context.Context, def *NodeDef, in HostInput) (*Output, []string, error) {
	limits := in.Limits.withDefaults()
	return sandboxRun(ctx, limits, func(L *lua.LState) (*Output, error) {
		h := &host{ctx: L.Context(), def: def, in: in, limits: limits, log: stateLog(L)}

		// The chunk is evaluated again for every run. It has to be: `run` is a
		// Lua closure, and a closure belongs to the state it was made in. The
		// happy consequence is that runs cannot contaminate each other — a
		// script that rewrote string.format or stashed data in a global starts
		// the next run with neither.
		if err := L.CallByParam(lua.P{Fn: L.NewFunctionFromProto(def.proto), NRet: 1, Protect: true}); err != nil {
			return nil, err
		}
		ret := L.Get(-1)
		L.Pop(1)
		tbl, ok := ret.(*lua.LTable)
		if !ok {
			return nil, fmt.Errorf("%s no longer returns a node definition", def.Source)
		}
		fn, ok := tbl.RawGetString("run").(*lua.LFunction)
		if !ok {
			return nil, fmt.Errorf("%s: run must be a function taking the node's ctx", def.Source)
		}

		if err := L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, h.contextTable(L)); err != nil {
			return nil, fmt.Errorf("%s: %w", def.Type, err)
		}
		result := L.Get(-1)
		L.Pop(1)
		return h.output(result)
	})
}

// contextTable builds the ctx a node's run function is called with.
func (h *host) contextTable(L *lua.LState) *lua.LTable {
	ctx := L.NewTable()
	ctx.RawSetString("input", inputTable(L, h.in.Input))
	ctx.RawSetString("config", toLua(L, h.in.Config, 0))
	ctx.RawSetString("log", L.NewFunction(h.luaLog))

	// A function is present only when the capability behind it is. Absent
	// rather than present-and-failing is the difference between a pack that
	// cannot read files and a pack that can find out whether it is being
	// watched: if ctx.readFile exists and refuses, a pack can probe for the
	// permission, branch on it, and behave when it is being examined.
	if h.def.Has(CapLLM) && h.in.LLM != nil {
		ctx.RawSetString("llm", L.NewFunction(h.luaLLM))
	}
	if h.def.Has(CapFiles) && h.in.Files != nil {
		ctx.RawSetString("readFile", L.NewFunction(h.luaReadFile))
		ctx.RawSetString("writeFile", L.NewFunction(h.luaWriteFile))
	}
	return ctx
}

// inputTable turns the upstream value into the shape a node reads:
// { text = "…" }, { image = { mimeType = …, dataUrl = … } } or { data = … }.
// An unconnected input is an empty table rather than nil, so `ctx.input.text`
// is nil instead of an error about indexing nil.
func inputTable(L *lua.LState, in *Output) *lua.LTable {
	tbl := L.NewTable()
	if in == nil {
		return tbl
	}
	switch in.Type {
	case "text":
		tbl.RawSetString("text", lua.LString(fmt.Sprint(orEmpty(in.Value["text"]))))
	case "image":
		img := L.NewTable()
		img.RawSetString("mimeType", lua.LString(fmt.Sprint(orEmpty(in.Value["mimeType"]))))
		img.RawSetString("dataUrl", lua.LString(fmt.Sprint(orEmpty(in.Value["dataUrl"]))))
		tbl.RawSetString("image", img)
	default:
		// json, boolean and number all carry their payload under "data"; a node
		// that wants to know which it was can look at the value.
		if data, ok := in.Value["data"]; ok {
			tbl.RawSetString("data", toLua(L, data, 0))
		} else {
			tbl.RawSetString("data", toLua(L, in.Value, 0))
		}
	}
	return tbl
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// ---------- the host functions ----------

func (h *host) luaLog(L *lua.LState) int {
	parts := make([]string, 0, L.GetTop())
	for i := 1; i <= L.GetTop(); i++ {
		parts = append(parts, L.ToStringMeta(L.Get(i)).String())
	}
	h.log.append(strings.Join(parts, "\t"))
	return 0
}

// luaLLM is ctx.llm. It is the expensive one: it spends the user's own account,
// and a loop calling it is the most likely way a bad pack does real damage, so
// it is counted and the refusal past the budget says what the budget was.
func (h *host) luaLLM(L *lua.LState) int {
	if h.llmCalls >= h.limits.MaxLLMCalls {
		L.RaiseError("ctx.llm: this node has already made %d model calls, which is the limit of %d for one node run",
			h.llmCalls, h.limits.MaxLLMCalls)
	}

	arg := L.CheckTable(1)
	req := LLMRequest{
		Prompt:    luaString(arg, "prompt"),
		System:    luaString(arg, "system"),
		Model:     luaString(arg, "model"),
		MaxTokens: 2000,
	}
	if n, ok := arg.RawGetString("maxTokens").(lua.LNumber); ok && int(n) > 0 {
		req.MaxTokens = int(n)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		L.RaiseError("ctx.llm needs a prompt")
	}

	// Counted before the call, not after: a call that fails still cost time and
	// may have cost tokens, and a node that retries in a loop must not be able
	// to spend the budget for free by making every call fail.
	h.llmCalls++
	text, err := h.in.LLM(h.ctx, req)
	if err != nil {
		L.RaiseError("ctx.llm failed: %s", err.Error())
	}
	L.Push(lua.LString(text))
	return 1
}

// luaReadFile is ctx.readFile. It exists only when the pack declared the files
// capability and the host supplied a FileAccess; the path goes straight to that
// gate, which is the only thing in the product that decides what a path means.
func (h *host) luaReadFile(L *lua.LState) int {
	path := L.CheckString(1)
	data, mime, err := h.in.Files.Read(path)
	if err != nil {
		L.RaiseError("ctx.readFile(%q): %s", path, err.Error())
	}
	if len(data) > h.limits.MaxFileBytes {
		L.RaiseError("ctx.readFile(%q): file is %d bytes, over the limit of %d a node may read",
			path, len(data), h.limits.MaxFileBytes)
	}
	L.Push(lua.LString(data))
	L.Push(lua.LString(mime))
	return 2
}

// luaWriteFile is ctx.writeFile, returning the path the gate actually wrote.
func (h *host) luaWriteFile(L *lua.LState) int {
	path := L.CheckString(1)
	contents := L.CheckString(2)
	if len(contents) > h.limits.MaxFileBytes {
		L.RaiseError("ctx.writeFile(%q): %d bytes, over the limit of %d a node may write",
			path, len(contents), h.limits.MaxFileBytes)
	}
	written, err := h.in.Files.Write(path, []byte(contents))
	if err != nil {
		L.RaiseError("ctx.writeFile(%q): %s", path, err.Error())
	}
	L.Push(lua.LString(written))
	return 1
}

// ---------- the result ----------

// output turns what run returned into a NodeOutput-shaped value, or explains
// why it is not one.
func (h *host) output(v lua.LValue) (*Output, error) {
	tbl, ok := v.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("%s: run must return a table like { text = \"…\" }, { image = { mimeType = …, dataUrl = … } } or { data = … }, got %s",
			h.def.Type, v.Type().String())
	}

	if text, ok := tbl.RawGetString("text").(lua.LString); ok {
		if len(text) > maxDataURLBytes {
			return nil, fmt.Errorf("%s returned %d bytes of text, over the limit of %d", h.def.Type, len(text), maxDataURLBytes)
		}
		return &Output{Type: "text", Value: map[string]any{"text": string(text)}}, nil
	}

	if img, ok := tbl.RawGetString("image").(*lua.LTable); ok {
		mime := luaString(img, "mimeType")
		dataURL := luaString(img, "dataUrl")
		if err := checkImageDataURL(mime, dataURL); err != nil {
			return nil, fmt.Errorf("%s: image: %w", h.def.Type, err)
		}
		return &Output{Type: "image", Value: map[string]any{
			"mimeType": mime,
			"dataUrl":  dataURL,
		}}, nil
	}

	if data := tbl.RawGetString("data"); data != lua.LNil {
		budget := maxValueNodes
		value, err := fromLua(data, 0, &budget)
		if err != nil {
			return nil, fmt.Errorf("%s: data: %w", h.def.Type, err)
		}
		return &Output{Type: "json", Value: map[string]any{"data": value}}, nil
	}

	return nil, fmt.Errorf("%s: run returned a table with none of text, image or data", h.def.Type)
}

// checkImageDataURL refuses anything but an inline image.
//
// The builder renders this value in the user's browser. An http(s) URL here
// would be a network request made by the app, on the user's network, with a
// path the pack chose — a beacon that says the pack ran and where, and a way to
// reach hosts only that machine can see. A pack has no network, and letting one
// smuggle a URL out through an image node would hand it one.
func checkImageDataURL(mime, dataURL string) error {
	if !strings.HasPrefix(mime, "image/") {
		return fmt.Errorf("mimeType %q is not an image type", mime)
	}
	if len(dataURL) > maxDataURLBytes {
		return fmt.Errorf("dataUrl is %d bytes, over the limit of %d", len(dataURL), maxDataURLBytes)
	}
	if !strings.HasPrefix(dataURL, "data:image/") {
		return fmt.Errorf("dataUrl must be an inline data: URL; a node cannot return a link for the app to fetch")
	}
	if !strings.Contains(dataURL[:min(len(dataURL), 128)], ";base64,") {
		return fmt.Errorf("dataUrl must be base64 encoded")
	}
	return nil
}
