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
// have been given as data, except the things a node is for, and every one of
// those is behind a capability its pack had to declare: a model call, the
// project folder the user already opened, the host making an image, the host
// describing one, and the agent loop.
//
// None of them is a wider power than the host already had. That is the rule
// this file is built on: a host function does work the engine was doing anyway,
// on data the engine already holds, and hands back the result. A pack never
// names an endpoint, never names a credential, and never sees a byte of the
// filesystem the path gate did not give it.

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

// NodeFunc is the other way a node reaches the host, and the one the engine's
// own built-ins use.
//
// LLMFunc lends a pack one model call and leaves the pack to decide what the
// answer means. A NodeFunc is the opposite: the host does the whole of some
// piece of work — generate an image, run an agent loop — and hands back the
// node output it produced. cfg is the table the script passed, converted with
// the same bounded conversion everything else crosses this boundary with.
//
// It returns an *Output rather than a value the script assembles, because the
// things behind these functions produce outputs a script could not assemble:
// an image node's result carries the byte count and the URL of the copy the
// host stored, and a Brain's carries the trace of what it did. Handing those
// back through a Lua table would lose them, so the script gets a receipt it can
// read and return, and the value itself never leaves Go. See resultTable.
type NodeFunc func(ctx context.Context, cfg map[string]any) (*Output, error)

// ImageFuncs is everything the image capability grants: the host making an
// image on the node's behalf. Every one of them is a Go implementation the
// engine already had, and none of the pixel work happens in Lua.
//
// The images they work on are the node's own image inputs, which the host
// already holds — they do not travel through the script. A node's config says
// what to do; the graph says what to do it to.
type ImageFuncs struct {
	Generate         NodeFunc
	Edit             NodeFunc
	RemoveBackground NodeFunc
	Rotate           NodeFunc
	Flip             NodeFunc
	// Compose builds one previewable value out of several images, which is what
	// the 3D preview node is.
	Compose NodeFunc
}

// VisionFuncs is the vision capability: images to a model, words back.
type VisionFuncs struct {
	Describe NodeFunc
}

// AgentFuncs is the agent capability: the Brain loop, and the list of host
// tools a Brain can be given.
//
// It is the widest capability in the product, and deliberately the one with the
// fewest functions. A Brain calls the model repeatedly and executes other nodes
// of the graph as tools, which means a pack granted this can reach node types
// it was not granted itself. Anyone installing a pack that asks for it is being
// asked to agree to that.
type AgentFuncs struct {
	Brain NodeFunc
	Tools NodeFunc
}

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
	// The capabilities. A nil one is a capability the host is not granting, and
	// the matching function is then absent from ctx rather than present and
	// failing — see contextTable for why that distinction is the whole point.
	//
	// LLM and Complete are both the llm capability: LLM is one completion a
	// pack interprets itself, Complete is the whole of the built-in llm node,
	// prompt fallback and json extraction included.
	LLM      LLMFunc
	Complete NodeFunc
	Image    ImageFuncs
	Vision   VisionFuncs
	Agent    AgentFuncs
	Files    FileAccess
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
	ctx    context.Context
	def    *NodeDef
	in     HostInput
	limits Limits
	log    *runLog
	// providerCalls counts every call that can reach the user's own provider
	// account, whichever function made it. They share one budget because they
	// share one bill: a loop over ctx.generateImage is more expensive than a
	// loop over ctx.llm, not less, and a separate allowance for each would mean
	// a node could spend five budgets instead of one.
	providerCalls int
	// err is the Go error a host function failed with, kept so that runNode can
	// return it as it was rather than as the Lua error it had to become on the
	// way out. A built-in's message is the message the user has always read,
	// and "generate image needs a prompt" must not turn into
	// "generateImage: nodes/generate_image.lua:22: generate image needs a prompt".
	err error
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
			// A failure that came out of a host function is reported as the host
			// wrote it. The Lua layer it travelled through added a chunk name
			// and a line number, and for the engine's own built-ins that is the
			// name of a file the person reading the message has never seen.
			//
			// The check on the text is what keeps that honest: a script can
			// pcall a host call, swallow the error and then fail for a reason
			// of its own, and that later failure is the script's, not the
			// host's. It is a containment test rather than an equality one
			// because what comes back has a chunk name and a line in front of
			// it and a Lua traceback behind it — which is precisely the
			// decoration being removed.
			if h.err != nil && strings.Contains(err.Error(), h.err.Error()) {
				return nil, h.err
			}
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
	//
	// The same reasoning is why presence tracks the declared capability and not
	// the host's configuration. A function that disappears when no provider is
	// configured would tell a pack something about the machine it is on.
	if h.def.Has(CapLLM) {
		h.bind(L, ctx, "llm", h.in.LLM != nil, h.luaLLM)
		h.bindNode(L, ctx, "complete", h.in.Complete, spends)
	}
	if h.def.Has(CapImage) {
		h.bindNode(L, ctx, "generateImage", h.in.Image.Generate, spends)
		h.bindNode(L, ctx, "editImage", h.in.Image.Edit, spends)
		h.bindNode(L, ctx, "removeBackground", h.in.Image.RemoveBackground, spends)
		// Turning, mirroring and composing are pure pixel work on this side of
		// the boundary: no provider, no bill. They are bounded by the node's own
		// time and memory budget like any other work, and counting them against
		// the model allowance would only mean a node that rotates six images
		// could not also call a model.
		h.bindNode(L, ctx, "rotateImage", h.in.Image.Rotate, local)
		h.bindNode(L, ctx, "flipImage", h.in.Image.Flip, local)
		h.bindNode(L, ctx, "composeImages", h.in.Image.Compose, local)
	}
	if h.def.Has(CapVision) {
		h.bindNode(L, ctx, "vision", h.in.Vision.Describe, spends)
	}
	if h.def.Has(CapAgent) {
		h.bindNode(L, ctx, "brain", h.in.Agent.Brain, spends)
		h.bindNode(L, ctx, "agentTools", h.in.Agent.Tools, local)
	}
	if h.def.Has(CapFiles) && h.in.Files != nil {
		ctx.RawSetString("readFile", L.NewFunction(h.luaReadFile))
		ctx.RawSetString("writeFile", L.NewFunction(h.luaWriteFile))
	}
	return ctx
}

// cost says whether one host function can reach the user's provider account.
type cost bool

const (
	spends cost = true
	local  cost = false
)

// bind installs a host function when the host actually supplied it.
func (h *host) bind(L *lua.LState, ctx *lua.LTable, name string, supplied bool, fn lua.LGFunction) {
	if supplied {
		ctx.RawSetString(name, L.NewFunction(fn))
	}
}

// bindNode installs one NodeFunc under a name, counting it against the model
// budget when it is one of the ones that spends money.
func (h *host) bindNode(L *lua.LState, ctx *lua.LTable, name string, fn NodeFunc, c cost) {
	if fn == nil {
		return
	}
	ctx.RawSetString(name, L.NewFunction(func(L *lua.LState) int {
		if c == spends {
			h.chargeProviderCall(L, "ctx."+name)
		}
		cfg := map[string]any{}
		if arg, ok := L.Get(1).(*lua.LTable); ok {
			budget := maxValueNodes
			value, err := fromLua(arg, 0, &budget)
			if err != nil {
				L.RaiseError("ctx.%s: %s", name, err.Error())
			}
			if m, ok := value.(map[string]any); ok {
				cfg = m
			}
		}
		out, err := fn(h.ctx, cfg)
		if err != nil {
			h.fail(L, err)
		}
		if out == nil {
			h.fail(L, fmt.Errorf("ctx.%s returned nothing", name))
		}
		L.Push(h.resultTable(L, out))
		return 1
	}))
}

// chargeProviderCall spends one unit of the model budget, or refuses.
//
// It is charged before the call rather than after, for the same reason ctx.llm
// always has been: a call that fails still cost time and may have cost tokens,
// and a node that retries in a loop must not be able to spend the budget for
// free by making every call fail.
func (h *host) chargeProviderCall(L *lua.LState, name string) {
	if h.providerCalls >= h.limits.MaxLLMCalls {
		L.RaiseError("%s: this node has already made %d calls that spend your model account, which is the limit of %d for one node run",
			name, h.providerCalls, h.limits.MaxLLMCalls)
	}
	h.providerCalls++
}

// fail records a host function's own error and stops the script.
func (h *host) fail(L *lua.LState, err error) {
	h.err = err
	L.RaiseError("%s", err.Error())
}

// resultTable is the receipt a host function hands back: a table the script can
// read a little of, carrying the real output where the script cannot reach it.
//
// The payload is deliberately not in it. A data URL is megabytes of base64, and
// a node that looped over ctx.generateImage copying each one into Lua would
// spend the memory watchdog's whole allowance on strings it has no use for. The
// metadata is what a script could act on; the value itself goes back out
// through output() exactly as the host built it, which is the only way a node
// keeps the byte count and the stored URL that the engine has always produced.
func (h *host) resultTable(L *lua.LState, out *Output) *lua.LTable {
	tbl := L.NewTable()
	tbl.RawSetString("type", lua.LString(out.Type))
	for _, key := range []string{"text", "mimeType", "url", "savedPath", "shape", "bytes"} {
		if v, ok := out.Value[key]; ok {
			tbl.RawSetString(key, toLua(L, v, 0))
		}
	}
	// Userdata with no metatable is inert: a sandboxed script cannot create
	// one (newproxy is gone and the debug library was never opened), cannot
	// give this one a metatable (setmetatable refuses anything but a table),
	// and cannot read what is inside it. Holding it and handing it back is the
	// only thing it can do with it, which is exactly what it is for.
	ud := L.NewUserData()
	ud.Value = out
	tbl.RawSetString(hostResultKey, ud)
	return tbl
}

// hostResultKey is where a receipt keeps the output it stands for. The name is
// reserved rather than hidden: a node returning its own table with this key can
// only put something in it that is not a receipt, and hostResult ignores that.
const hostResultKey = "__zyvroHostResult"

// hostResult returns the output a table is a receipt for, if it is one.
func hostResult(tbl *lua.LTable) (*Output, bool) {
	ud, ok := tbl.RawGetString(hostResultKey).(*lua.LUserData)
	if !ok {
		return nil, false
	}
	out, ok := ud.Value.(*Output)
	return out, ok
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

	h.chargeProviderCall(L, "ctx.llm")
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

	// A receipt from a host function first, and before anything is read off the
	// table: a Brain's receipt carries a "text" field a script can look at, and
	// reading that instead of the output behind it would quietly throw away the
	// trace and the stored image the Brain actually produced.
	if out, ok := hostResult(tbl); ok {
		return out, nil
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
