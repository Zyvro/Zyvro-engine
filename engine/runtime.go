package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Zyvro/Zyvro-engine/plugins"
	"github.com/Zyvro/Zyvro-engine/providers"
)

// NodeOutput is the value produced by one node. Media is carried as data
// URLs inside Value so the engine stays JSON-serializable end to end.
// NodeOutput is plugins.Output, not a copy of it.
//
// They were two structs with the same two fields and the same two JSON tags,
// converted field by field at three places. Nothing kept them in step: add a
// field to one and the conversions compile unchanged, silently dropping it —
// which for a node's result means a value that leaves the node and never
// arrives. Aliasing removes the question rather than answering it, and it
// costs nothing here because engine already imports plugins.
//
// The Type is the port type system: text, image, json, boolean, number.
type NodeOutput = plugins.Output

func textOutput(s string) *NodeOutput {
	return &NodeOutput{Type: "text", Value: map[string]any{"text": s}}
}

func imageOutput(mime string, data []byte) *NodeOutput {
	return &NodeOutput{Type: "image", Value: map[string]any{
		"mimeType": mime,
		"dataUrl":  imageDataURL(mime, data),
		"bytes":    len(data),
	}}
}

func jsonOutput(v any) *NodeOutput {
	return &NodeOutput{Type: "json", Value: map[string]any{"data": v}}
}

// Runtime carries per-execution state through the graph.
type Runtime struct {
	ExecID     string
	Graph      *Graph
	Outputs    map[string]*NodeOutput // nodeID -> output
	Providers  *providers.Config
	Storage    MediaStore
	Inputs     map[string]any // runtime inputs keyed by node id or label
	AgentTrace []AgentStep
	External   ExternalTools // optional host tools for Brain nodes
	// Files is the project folder the file nodes read and write. It is nil
	// everywhere the engine does not run on the user's own machine, and the
	// file nodes refuse to run rather than reach for a folder that is not
	// there (see files.go).
	Files FileAccess

	// Plugins holds the node types this host can run: the bundled pack the
	// engine ships, and whatever the project installed on top of it. A type the
	// switch in executeWithInput has no case for is looked up here before the
	// run is failed, which is how a pack's node runs exactly like a built-in —
	// and, now, how a built-in runs at all. Nil falls back to a registry holding
	// the bundled pack alone, so a caller that never heard of packs still has
	// every node the engine ships. Build one with NewRegistry.
	Plugins *plugins.Registry
	// PluginLimits is the budget one plugin node runs under. The zero value
	// means plugins.DefaultLimits(), and every field is overridable
	// independently, so a host can lengthen the timeout without having to
	// restate the memory ceiling it did not mean to change.
	PluginLimits plugins.Limits
	// PluginLogs is what each plugin node printed, keyed by node id. A pack
	// node is code the user wrote, so its own print output is the only account
	// of why it did what it did; it is carried here the way AgentTrace carries
	// the Brain's steps, and pushed to the reporter as each node finishes.
	PluginLogs map[string][]string

	// Replay cache: fingerprints of the current graph state and outputs of
	// previously completed nodes. A node whose fingerprint matches a cache
	// entry is replayed instead of executed (no model call). Nil disables.
	Fingerprints map[string]NodeFingerprint
	Cache        map[string]CacheEntry
	// Replayed records which node ids were served from the cache.
	Replayed map[string]bool
}

type AgentStep struct {
	Step        int    `json:"step"`
	Tool        string `json:"tool"`
	Summary     string `json:"summary"`
	Status      string `json:"status"`
	Args        string `json:"args,omitempty"`
	Result      string `json:"result,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
}

// ExternalTools lets the host expose extra function tools to Brain nodes
// through a "zyvroTools" node (e.g. the Zyvro MCP tools: list, inspect and
// run other workflows). The engine stays independent from the API layer.
type ExternalTools interface {
	Schemas() []providers.ToolSchema
	// Call runs one tool; images are URLs of media produced by the call and
	// execID (optional) identifies a workflow execution it started.
	Call(ctx context.Context, name string, args map[string]any) (text string, images []string, execID string, err error)
}

// MediaStore persists generated media outside of node outputs (raw files).
type MediaStore interface {
	SaveMedia(execID, nodeID, filename string, data []byte, mime string) (url string, err error)
}

// RunInput are the resolved inputs for a single node execution.
type RunInput struct {
	Node *GraphNode
	// Upstream outputs ordered by edge id.
	Upstream []*NodeOutput
	// Config is the node's data.config map (or data itself for simple nodes).
	Config map[string]any
}

func nodeConfig(n *GraphNode) map[string]any {
	if n.Data == nil {
		return map[string]any{}
	}
	if cfg, ok := n.Data["config"].(map[string]any); ok {
		return cfg
	}
	return n.Data
}

// upstreamOutputs returns outputs of data-edge upstream nodes.
func (r *Runtime) upstreamOutputs(n *GraphNode) ([]*NodeOutput, error) {
	var out []*NodeOutput
	for _, src := range UpstreamOf(r.Graph, n.ID) {
		o, ok := r.Outputs[src]
		if !ok {
			return nil, fmt.Errorf("upstream node %s has no output", src)
		}
		out = append(out, o)
	}
	return out, nil
}

// firstUpstream returns the first upstream output of the given type, or nil.
func firstUpstream(outs []*NodeOutput, typ string) *NodeOutput {
	for _, o := range outs {
		if o != nil && o.Type == typ {
			return o
		}
	}
	return nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func strDefault(v any, def string) string {
	s := str(v)
	if s == "" {
		return def
	}
	return s
}

func num(v any, def float64) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return def
}

// resolveInputs substitutes {{input:<key>}} placeholders in a string using
// runtime inputs.
func (r *Runtime) resolveInputs(s string) string {
	for key, v := range r.Inputs {
		placeholder := "{{input:" + key + "}}"
		if strings.Contains(s, placeholder) {
			s = strings.ReplaceAll(s, placeholder, fmt.Sprint(v))
		}
	}
	return s
}

// ExecuteNode runs a single node and returns its output.
func (r *Runtime) ExecuteNode(ctx context.Context, n *GraphNode) (*NodeOutput, error) {
	ups, err := r.upstreamOutputs(n)
	if err != nil {
		return nil, err
	}
	return r.executeWithInput(ctx, &RunInput{Node: n, Upstream: ups, Config: nodeConfig(n)})
}

// executeWithInput dispatches a node execution with pre-resolved inputs.
// Used both by the DAG loop and by the Brain when invoking tool nodes.
//
// What is left here is what could not be Lua, and each one is here for a reason
// that is about the host rather than about the node:
//
//   - textInput and imageInput read the run's inputs, keyed by the node's own
//     input key, id or label. That map is the caller's, not the graph's, and a
//     node that could reach it could read every value a run was started with.
//   - fileInput and fileOutput are the project folder, through a path gate with
//     rules about symlinks and .zyvro that exist in exactly one place. The
//     sandbox's file capability is narrower than these nodes are — it reads
//     text, where fileInput decides between text, JSON and an image and refuses
//     binary that merely decodes — so expressing them in Lua would have meant
//     widening the gate rather than using it.
//   - preview and output hand their input straight back, the same pointer with
//     every key it arrived with. A value that crossed into Lua and back would
//     come back as much of itself as the conversion could carry, which is not
//     the same thing: savedPath, trace and faces would all be gone.
//   - mergeText joins the whole upstream list, and encodes anything that is not
//     text as the JSON of its raw node value. A Lua version would need every
//     upstream value rather than the first, un-narrowed, plus a JSON encoder —
//     three additions to the host API for a node whose behaviour is a join.
//   - generateVideo refuses to run. It is deliberately absent from the palette,
//     and a Lua file for it would put it back: a node nobody can place is
//     better than one that can be placed and then fails, but an old workflow
//     that has one still deserves the real explanation.
//
// Everything else is a Lua file in the bundled pack, and reaches this function
// through its default case.
func (r *Runtime) executeWithInput(ctx context.Context, in *RunInput) (*NodeOutput, error) {
	switch in.Node.Type {
	case "textInput":
		return r.runTextInput(in)
	case "imageInput":
		return r.runImageInput(in)
	case "fileInput":
		return r.runFileInput(in)
	case "fileOutput":
		return r.runFileOutput(in)
	case "mergeText":
		return r.runMergeText(in)
	case "preview", "output":
		var o *NodeOutput
		for _, u := range in.Upstream {
			if u != nil {
				o = u
				break
			}
		}
		if o == nil {
			return nil, fmt.Errorf("%s node has no input", in.Node.Type)
		}
		return o, nil
	case "generateVideo":
		return nil, fmt.Errorf("video generation is disabled in this build")
	default:
		// One of the bundled pack's nodes, or one an installed pack
		// contributed, and by this point the two run the same way. Only once
		// the registry has no such type either is the graph actually wrong.
		return r.runPluginNode(ctx, in)
	}
}

// ---------- Input nodes ----------

// InputKey is the public name of a runtime input node: the key callers use
// in the run inputs map (UI run panel, REST, MCP). Empty means the node is
// only addressable by node id / label (legacy).
func InputKey(n *GraphNode) string {
	if cfg, ok := n.Data["config"].(map[string]any); ok {
		return strings.TrimSpace(str(cfg["inputKey"]))
	}
	return ""
}

// runtimeInput looks up the caller-supplied value for an input node, trying
// the input key first, then node id, then label. Values may be a plain
// string or an object like {"text": "..."} / {"url": "..."} / {"dataUrl": "..."}.
func (r *Runtime) runtimeInput(n *GraphNode) (string, bool) {
	for _, key := range []string{InputKey(n), n.ID, str(n.Data["label"])} {
		if key == "" {
			continue
		}
		v, ok := r.Inputs[key]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			return t, true
		case map[string]any:
			for _, k := range []string{"text", "value", "dataUrl", "url"} {
				if s := str(t[k]); s != "" {
					return s, true
				}
			}
			return "", true
		default:
			return fmt.Sprint(v), true
		}
	}
	return "", false
}

func (r *Runtime) runTextInput(in *RunInput) (*NodeOutput, error) {
	if v, ok := r.runtimeInput(in.Node); ok {
		return textOutput(v), nil
	}
	value := r.resolveInputs(str(in.Config["value"]))
	return textOutput(value), nil
}

func (r *Runtime) runImageInput(in *RunInput) (*NodeOutput, error) {
	src, provided := r.runtimeInput(in.Node)
	if !provided {
		src = str(in.Config["dataUrl"])
	}
	if src == "" {
		if key := InputKey(in.Node); key != "" {
			return nil, fmt.Errorf("image input %q was not provided", key)
		}
		return nil, fmt.Errorf("image input node has no image")
	}
	mime, data, err := loadImageSource(src)
	if err != nil {
		return nil, fmt.Errorf("image input invalid: %w", err)
	}
	return imageOutput(mime, data), nil
}

// loadImageSource accepts a data URL or an http(s) URL (fetched with a
// short timeout) and returns the decoded bytes.
func loadImageSource(src string) (string, []byte, error) {
	if strings.HasPrefix(src, "data:") {
		mime, data, err := parseDataURL(src)
		if err != nil || data == nil {
			return "", nil, fmt.Errorf("not a valid data URL")
		}
		return mime, data, nil
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(src)
		if err != nil {
			return "", nil, fmt.Errorf("fetch failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return "", nil, fmt.Errorf("fetch failed: HTTP %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 25<<20))
		if err != nil {
			return "", nil, err
		}
		mime := resp.Header.Get("Content-Type")
		if i := strings.Index(mime, ";"); i >= 0 {
			mime = mime[:i]
		}
		if !strings.HasPrefix(mime, "image/") {
			mime = http.DetectContentType(data)
		}
		if !strings.HasPrefix(mime, "image/") {
			return "", nil, fmt.Errorf("URL did not return an image (%s)", mime)
		}
		return mime, data, nil
	}
	return "", nil, fmt.Errorf("expected a data URL or an http(s) URL")
}

// ---------- Utility nodes ----------

func (r *Runtime) runMergeText(in *RunInput) (*NodeOutput, error) {
	sep := strDefault(in.Config["separator"], "\n")
	sep = strings.ReplaceAll(sep, "\\n", "\n")
	var parts []string
	for _, u := range in.Upstream {
		if u == nil {
			continue
		}
		if u.Type == "text" {
			parts = append(parts, str(u.Value["text"]))
		} else {
			b, _ := json.Marshal(u.Value)
			parts = append(parts, string(b))
		}
	}
	return textOutput(strings.Join(parts, sep)), nil
}

// runVoxelPreview aggregates every upstream image into a json output the
// builder renders as a textured 3D primitive (cube: one image per face,
// sphere: equirectangular longitude bands). It executes no model: it is a
// visual test harness, so the output keeps the full image data URLs.
func (r *Runtime) runVoxelPreview(in *RunInput) (*NodeOutput, error) {
	shape := strDefault(in.Config["shape"], "cube")
	faces := []any{}
	for _, u := range in.Upstream {
		if u == nil || u.Type != "image" {
			continue
		}
		dataUrl := str(u.Value["dataUrl"])
		if dataUrl == "" {
			continue
		}
		face := map[string]any{
			"dataUrl":  dataUrl,
			"mimeType": str(u.Value["mimeType"]),
			"bytes":    num(u.Value["bytes"], 0),
		}
		if url, ok := u.Value["url"].(string); ok && url != "" {
			face["url"] = url
		}
		faces = append(faces, face)
	}
	if len(faces) == 0 {
		return nil, fmt.Errorf("voxel preview needs at least one image input")
	}
	return &NodeOutput{
		Type: "image",
		Value: map[string]any{
			"mimeType": "image/png",
			// The aggregate (not one face) is the node's previewable output.
			"dataUrl": str(faces[0].(map[string]any)["dataUrl"]),
			"shape":   shape,
			"faces":   faces,
		},
	}, nil
}

// runHostTools is the zyvroTools node: it does nothing in the data flow, and
// the Brain binds the host's tools through the tool edge. What it produces is
// the list of names that were bound, so that a node with no ports still tells
// somebody looking at it what it did.
func (r *Runtime) runHostTools(_ *RunInput) (*NodeOutput, error) {
	names := []string{}
	if r.External != nil {
		for _, t := range r.External.Schemas() {
			names = append(names, t.Name)
		}
	}
	return jsonOutput(map[string]any{"tools": names}), nil
}

// ---------- AI nodes ----------

// The AI implementations below are no longer reached from the switch. Each is
// the Go behind one host function, called by the bundled pack's Lua node of the
// same name, and the settings they read are the ones that node passed. Those
// have already been through the placeholder resolver on their way into the
// script, which is why nothing here resolves a config value a second time; the
// text arriving on an input has not, so that is still resolved where it is read.
func (r *Runtime) runLLM(ctx context.Context, in *RunInput) (*NodeOutput, error) {
	system := str(in.Config["system"])
	prompt := str(in.Config["prompt"])
	if prompt == "" {
		if t := firstUpstream(in.Upstream, "text"); t != nil {
			prompt = r.resolveInputs(str(t.Value["text"]))
		}
	}
	if prompt == "" {
		return nil, fmt.Errorf("LLM node needs a prompt (config or text input)")
	}

	messages := []providers.Message{}
	if system != "" {
		messages = append(messages, providers.Message{Role: "system", Content: system})
	}
	messages = append(messages, providers.Message{Role: "user", Content: prompt})

	resp, err := r.Providers.LLMComplete(ctx, providers.LLMRequest{
		Messages: messages,
		// A node may pin its own provider and model; empty falls back to the
		// deployment default.
		Provider:    str(in.Config["provider"]),
		Model:       str(in.Config["model"]),
		MaxTokens:   int(num(in.Config["maxTokens"], 2000)),
		Temperature: num(in.Config["temperature"], 0.7),
	})
	if err != nil {
		return nil, fmt.Errorf("LLM call failed: %w", err)
	}
	if b, ok := in.Config["jsonOutput"].(bool); ok && b {
		return jsonOutput(extractJSON(resp.Content)), nil
	}
	return textOutput(resp.Content), nil
}

func extractJSON(s string) any {
	for i := 0; i < len(s); i++ {
		if s[i] == '{' || s[i] == '[' {
			for j := len(s); j > i; j-- {
				if s[j-1] == '}' || s[j-1] == ']' {
					candidate := s[i:j]
					var v any
					if err := json.Unmarshal([]byte(candidate), &v); err == nil {
						return v
					}
				}
			}
		}
	}
	return map[string]any{"raw": s}
}

func (r *Runtime) runGenerateImage(ctx context.Context, in *RunInput) (*NodeOutput, error) {
	prompt := str(in.Config["prompt"])
	if prompt == "" {
		if t := firstUpstream(in.Upstream, "text"); t != nil {
			prompt = r.resolveInputs(str(t.Value["text"]))
		}
	}
	if prompt == "" {
		return nil, fmt.Errorf("generate image needs a prompt")
	}
	// The deployment's image model is Gemini's — its environment variable says
	// so — and handing that name to Black Forest Labs or to a diffusion server
	// on this machine would be handing over a name nobody there has heard of.
	// An empty string is a real answer: it lets the backend use its own.
	model := str(in.Config["model"])
	if model == "" && r.Providers != nil && r.Providers.ResolvedImageProvider(str(in.Config["provider"])) == "google" {
		model = r.imageModel()
	}
	aspect := strDefault(in.Config["aspectRatio"], "1:1")
	imageSize := strDefault(in.Config["imageSize"], "1K")

	var refs []providers.ImageResult
	for _, u := range in.Upstream {
		if u == nil || u.Type != "image" {
			continue
		}
		mime, data, err := parseDataURL(str(u.Value["dataUrl"]))
		if err != nil || data == nil {
			continue
		}
		refs = append(refs, providers.ImageResult{Data: data, MimeType: mime})
	}

	img, err := r.Providers.ImageGenerate(ctx, str(in.Config["provider"]), model, prompt, aspect, imageSize, refs)
	if err != nil {
		return nil, fmt.Errorf("image generation failed: %w", err)
	}
	return r.storeImage(in.Node.ID, "image", img), nil
}

func extForMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	default:
		return "png"
	}
}

// storeImage wraps generated bytes in a NodeOutput, saving a raw copy to
// storage when available (Value["url"] then points at the stored file).
func (r *Runtime) storeImage(nodeID, prefix string, img *providers.ImageResult) *NodeOutput {
	out := imageOutput(img.MimeType, img.Data)
	if r.Storage != nil {
		url, err := r.Storage.SaveMedia(r.ExecID, nodeID, prefix+"."+extForMime(img.MimeType), img.Data, img.MimeType)
		if err == nil && url != "" {
			out.Value["url"] = url
		}
	}
	return out
}

func (r *Runtime) runEditImage(ctx context.Context, in *RunInput) (*NodeOutput, error) {
	src := firstUpstream(in.Upstream, "image")
	if src == nil {
		return nil, fmt.Errorf("edit image needs an image input")
	}
	mime, data, err := parseDataURL(str(src.Value["dataUrl"]))
	if err != nil || data == nil {
		return nil, fmt.Errorf("edit image source invalid: %w", err)
	}
	prompt := str(in.Config["prompt"])
	if prompt == "" {
		if t := firstUpstream(in.Upstream, "text"); t != nil {
			prompt = str(t.Value["text"])
		}
	}
	if prompt == "" {
		return nil, fmt.Errorf("edit image needs a prompt")
	}
	// Editing was Gemini's alone, on the reasoning that it had no equivalent
	// elsewhere. It has: an image and a prompt in, an image out, is what a
	// FLUX.2 Klein does on somebody's own machine and what Black Forest Labs'
	// hosted API does too. Routed like generation, which is the same call with
	// the source as its reference.
	provider := str(in.Config["provider"])
	model := str(in.Config["model"])
	if model == "" && r.Providers != nil && r.Providers.ResolvedImageProvider(provider) == "google" {
		model = r.imageModel()
	}
	img, err := r.Providers.ImageGenerate(ctx, provider, model, prompt, "", "1K",
		[]providers.ImageResult{{Data: data, MimeType: mime}})
	if err != nil {
		return nil, fmt.Errorf("image edit failed: %w", err)
	}
	return r.storeImage(in.Node.ID, "edit", img), nil
}

// runRemoveBackground has two modes. "ai" (default): the two-pass matte
// protocol — edit the source onto a pure white matte, then edit the
// white-pass result onto a pure black matte (so the subject stays
// pixel-identical across passes), then recover alpha per-pixel by comparing
// the two passes (see matte.go). "programmatic": detect the background
// colors from the image border and erase that color range with a smooth
// alpha ramp — pure Go, no model call, instant and free.
func (r *Runtime) runRemoveBackground(ctx context.Context, in *RunInput) (*NodeOutput, error) {
	src := firstUpstream(in.Upstream, "image")
	if src == nil {
		return nil, fmt.Errorf("remove background needs an image input")
	}
	mime, data, err := parseDataURL(str(src.Value["dataUrl"]))
	if err != nil || data == nil {
		return nil, fmt.Errorf("remove background source invalid: %w", err)
	}

	if strDefault(in.Config["mode"], "ai") == "programmatic" {
		img, _, err := decodeImage(data)
		if err != nil {
			return nil, fmt.Errorf("remove background source decode failed: %w", err)
		}
		bgs := detectBackgroundColors(img)
		if len(bgs) == 0 {
			return nil, fmt.Errorf("no uniform background detected on the image border")
		}
		tolerance := num(in.Config["tolerance"], 30)
		transparent := removeBackgroundRange(img, bgs, tolerance)
		pngData, err := encodePNG(transparent)
		if err != nil {
			return nil, fmt.Errorf("png encode failed: %w", err)
		}
		return r.storeImage(in.Node.ID, "transparent", &providers.ImageResult{Data: pngData, MimeType: "image/png"}), nil
	}

	model := strDefault(in.Config["model"], r.imageModel())
	source := providers.ImageResult{Data: data, MimeType: mime}

	const keep = "Keep the subject exactly unchanged. Do not alter shape, composition, position, colors, glow, softness, opacity, antialiasing, or details. Do not add or remove any element. Return one final image only."
	const whitePrompt = "Change the background to a perfectly uniform pure white #FFFFFF background. " + keep
	const blackPrompt = "Change the background to a perfectly uniform pure black #000000 background. " + keep
	const whiteForce = "Replace the ENTIRE background with a flat, solid, pure white (#FFFFFF) color, edge to edge, no gradient, no shadow, no texture. Every pixel that is not part of the subject must be exactly white. " + keep
	const blackForce = "Replace the ENTIRE background with a flat, solid, pure black (#000000) color, edge to edge, no gradient, no vignette, no texture. Every pixel that is not part of the subject must be exactly black. " + keep

	// pass runs one background edit and validates the result by looking at
	// the image border: the model sometimes returns the input unchanged.
	pass := func(name, prompt string, ref providers.ImageResult, wantDark bool) (*providers.ImageResult, image.Image, float64, error) {
		res, err := r.Providers.GeminiImageGenerate(ctx, model, prompt, "", "1K", []providers.ImageResult{ref})
		if err != nil {
			return nil, nil, 0, fmt.Errorf("%s matte pass failed: %w", name, err)
		}
		img, _, err := decodeImage(res.Data)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("%s pass decode failed: %w", name, err)
		}
		luma := borderLuma(img)
		ok := luma >= 200
		if wantDark {
			ok = luma <= 40
		}
		if !ok {
			return res, img, luma, fmt.Errorf("%s matte pass did not change the background (border luminance %.0f)", name, luma)
		}
		return res, img, luma, nil
	}

	// White pass from the source, retried with a firmer prompt.
	whiteImg, w, _, err := pass("white", whitePrompt, source, false)
	if err != nil && whiteImg != nil {
		whiteImg, w, _, err = pass("white", whiteForce, source, false)
	}
	if err != nil {
		return nil, err
	}

	// Black pass: from the white output first (identical subject), then from
	// the source, each with a firmer prompt as fallback.
	whiteRef := providers.ImageResult{Data: whiteImg.Data, MimeType: whiteImg.MimeType}
	var blackImg *providers.ImageResult
	var b image.Image
	for _, attempt := range []struct {
		prompt string
		ref    providers.ImageResult
	}{
		{blackPrompt, whiteRef}, {blackForce, whiteRef}, {blackForce, source},
	} {
		blackImg, b, _, err = pass("black", attempt.prompt, attempt.ref, true)
		if err == nil {
			break
		}
		if blackImg == nil {
			return nil, err // transport/decode error: do not retry blindly
		}
	}
	if err != nil {
		return nil, err
	}

	// Keep both passes next to the result for inspection.
	if r.Storage != nil {
		_, _ = r.Storage.SaveMedia(r.ExecID, in.Node.ID, "pass-white."+extForMime(whiteImg.MimeType), whiteImg.Data, whiteImg.MimeType)
		_, _ = r.Storage.SaveMedia(r.ExecID, in.Node.ID, "pass-black."+extForMime(blackImg.MimeType), blackImg.Data, blackImg.MimeType)
	}

	transparent, err := mattingExtract(w, b)
	if err != nil {
		return nil, fmt.Errorf("alpha extraction failed: %w", err)
	}
	pngData, err := encodePNG(transparent)
	if err != nil {
		return nil, fmt.Errorf("png encode failed: %w", err)
	}
	return r.storeImage(in.Node.ID, "transparent", &providers.ImageResult{Data: pngData, MimeType: "image/png"}), nil
}

// runRotateImage rotates the upstream image by a multiple of 90 degrees
// (config "degrees", default 90). Pure Go, no model call.
func (r *Runtime) runRotateImage(in *RunInput) (*NodeOutput, error) {
	src := firstUpstream(in.Upstream, "image")
	if src == nil {
		return nil, fmt.Errorf("rotate image needs an image input")
	}
	_, data, err := parseDataURL(str(src.Value["dataUrl"]))
	if err != nil || data == nil {
		return nil, fmt.Errorf("rotate image source invalid: %w", err)
	}
	img, _, err := decodeImage(data)
	if err != nil {
		return nil, fmt.Errorf("rotate image source decode failed: %w", err)
	}
	degrees := int(num(in.Config["degrees"], 90))
	rotated := rotateImage(img, degrees)
	pngData, err := encodePNG(rotated)
	if err != nil {
		return nil, fmt.Errorf("png encode failed: %w", err)
	}
	return r.storeImage(in.Node.ID, "rotated", &providers.ImageResult{Data: pngData, MimeType: "image/png"}), nil
}

// runFlipImage mirrors the upstream image horizontally or vertically
// (config "axis": "h" or "v", default "h"). Pure Go, no model call.
func (r *Runtime) runFlipImage(in *RunInput) (*NodeOutput, error) {
	src := firstUpstream(in.Upstream, "image")
	if src == nil {
		return nil, fmt.Errorf("flip image needs an image input")
	}
	_, data, err := parseDataURL(str(src.Value["dataUrl"]))
	if err != nil || data == nil {
		return nil, fmt.Errorf("flip image source invalid: %w", err)
	}
	img, _, err := decodeImage(data)
	if err != nil {
		return nil, fmt.Errorf("flip image source decode failed: %w", err)
	}
	axis := strDefault(in.Config["axis"], "h")
	flipped := flipImage(img, axis)
	pngData, err := encodePNG(flipped)
	if err != nil {
		return nil, fmt.Errorf("png encode failed: %w", err)
	}
	return r.storeImage(in.Node.ID, "flipped", &providers.ImageResult{Data: pngData, MimeType: "image/png"}), nil
}

func (r *Runtime) runVision(ctx context.Context, in *RunInput) (*NodeOutput, error) {
	instruction := str(in.Config["instruction"])
	if instruction == "" {
		if t := firstUpstream(in.Upstream, "text"); t != nil {
			instruction = str(t.Value["text"])
		}
	}
	if instruction == "" {
		return nil, fmt.Errorf("vision node needs an instruction")
	}
	var images [][]byte
	var mimes []string
	for _, u := range in.Upstream {
		if u == nil || u.Type != "image" {
			continue
		}
		mime, data, err := parseDataURL(str(u.Value["dataUrl"]))
		if err != nil || data == nil {
			continue
		}
		images = append(images, data)
		mimes = append(mimes, mime)
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("vision node needs at least one image")
	}
	// The provider is the node's choice, or whichever credential the run
	// actually carries. It used to be Gemini and nothing else, which meant an
	// account holding an Ollama key — or a run on the free allowance, which
	// lends exactly that — was told to go and get a Google key for a job its
	// own credential could do. Most models Ollama serves today are
	// vision-capable.
	provider := str(in.Config["provider"])
	model := str(in.Config["model"])
	if model == "" && r.Providers != nil && r.Providers.ResolvedVisionProvider(provider) == "google" {
		// Only Gemini gets the Gemini default. Handing that name to Ollama
		// would be handing it a name nobody there has heard of.
		model = r.visionModel()
	}
	resp, err := r.Providers.VisionAsk(ctx, provider, model, instruction, images, mimes)
	if err != nil {
		return nil, fmt.Errorf("vision call failed: %w", err)
	}
	if b, ok := in.Config["jsonOutput"].(bool); ok && b {
		return jsonOutput(extractJSON(resp)), nil
	}
	return textOutput(resp), nil
}

// NewRuntime builds a Runtime for an execution.
func NewRuntime(execID string, g *Graph, p *providers.Config, store MediaStore, inputs map[string]any) *Runtime {
	if inputs == nil {
		inputs = map[string]any{}
	}
	return &Runtime{
		ExecID:    execID,
		Graph:     g,
		Outputs:   map[string]*NodeOutput{},
		Providers: p,
		Storage:   store,
		Inputs:    inputs,
		Replayed:  map[string]bool{},
	}
}

// NodeReporter lets the API layer persist per-node status without the engine
// knowing about MongoDB.
type NodeReporter interface {
	// NodeStart records a node starting; returns a finish function.
	NodeStart(nodeID, nodeType string) func(output *NodeOutput, err error)
}

// NodeLogger is the optional half of NodeReporter, the way DirCreatingWriter is
// the optional half of FileAccess.
//
// Only a plugin node produces a log: it is user-written Lua, and what it
// printed is the whole of the explanation when it misbehaves. A reporter that
// can persist that implements this and is handed the lines as each node
// finishes, including when it finished by failing — which is the case that
// matters, because a failing run stops there and nothing later would flush
// them. A reporter that cannot simply does not implement it, which is why this
// is a second interface rather than a wider NodeStart: the hosted API's
// reporter has nowhere to put a log and should not have to say so.
type NodeLogger interface {
	NodeLog(nodeID string, lines []string)
}

// ExecuteWithReporting runs the graph, persisting node status via reporter.
// Nodes served from the replay cache record status "cached".
func (r *Runtime) ExecuteWithReporting(ctx context.Context, rep NodeReporter) error {
	order, err := TopoOrder(r.Graph)
	if err != nil {
		return err
	}
	for _, id := range order {
		n := NodeByID(r.Graph, id)
		if n == nil {
			return fmt.Errorf("missing node %s", id)
		}
		// Cache replay: a node whose fingerprint matches a previously
		// completed run is served from the cache without executing.
		if entry, ok := r.cachedOutput(id); ok {
			r.Outputs[id] = entry.Output
			r.Replayed[id] = true
			finish := rep.NodeStart(id, n.Type)
			finish(entry.Output, nil)
			continue
		}
		finish := rep.NodeStart(id, n.Type)
		out, err := r.ExecuteNode(ctx, n)
		if lg, ok := rep.(NodeLogger); ok {
			if lines := r.PluginLogs[id]; len(lines) > 0 {
				lg.NodeLog(id, lines)
			}
		}
		if err != nil {
			finish(nil, err)
			return fmt.Errorf("node %s (%s): %w", id, n.Type, err)
		}
		r.Outputs[id] = out
		finish(out, nil)
	}
	return nil
}

// cachedOutput returns the cache entry for a node when replay is possible:
// the runtime has fingerprints + cache loaded, the node has a fingerprint,
// and a completed cache entry exists under that exact fingerprint.
func (r *Runtime) cachedOutput(nodeID string) (CacheEntry, bool) {
	if r.Fingerprints == nil || r.Cache == nil {
		return CacheEntry{}, false
	}
	// A fingerprint describes the graph, not the world, and some nodes are not
	// wholly described by the graph. Those are never served from the cache.
	if n := NodeByID(r.Graph, nodeID); n != nil && r.outsideTheFingerprint(n.Type) {
		return CacheEntry{}, false
	}
	fp, ok := r.Fingerprints[nodeID]
	if !ok {
		return CacheEntry{}, false
	}
	entry, ok := r.Cache[nodeID]
	if !ok || entry.Fingerprint != fp || entry.Output == nil {
		return CacheEntry{}, false
	}
	return entry, true
}

// Execute runs the whole graph in topological order.
func (r *Runtime) Execute(ctx context.Context) error {
	order, err := TopoOrder(r.Graph)
	if err != nil {
		return err
	}
	for _, id := range order {
		n := NodeByID(r.Graph, id)
		if n == nil {
			return fmt.Errorf("missing node %s", id)
		}
		out, err := r.ExecuteNode(ctx, n)
		if err != nil {
			return fmt.Errorf("node %s (%s): %w", id, n.Type, err)
		}
		r.Outputs[id] = out
	}
	return nil
}

// imageModel and visionModel are the platform defaults for nodes that do not
// name a model themselves. They come from the provider config so an operator
// can change them without a rebuild.
func (r *Runtime) imageModel() string {
	if r.Providers != nil && r.Providers.ImageModel != "" {
		return r.Providers.ImageModel
	}
	return "gemini-3.1-flash-image"
}

func (r *Runtime) visionModel() string {
	if r.Providers != nil && r.Providers.VisionModel != "" {
		return r.Providers.VisionModel
	}
	return "gemini-3.6-flash"
}
