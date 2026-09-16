package engine

import (
	"context"
	"fmt"

	"github.com/Zyvro/Zyvro-engine/plugins"
	"github.com/Zyvro/Zyvro-engine/providers"
)

// The bridge between the graph and a pack's Lua nodes — including the engine's
// own, which are a pack embedded in the binary.
//
// The engine imports plugins and never the other way round, so everything that
// has to cross — the upstream value, the resolved config, the capabilities — is
// assembled here and handed over as plain data. What a node may reach is decided
// on this side: the sandbox enforces the budget, but the decision to lend it the
// user's model account, their project folder, or the host's own image and agent
// work is the runtime's, and it is made once, in pluginHostInput.
//
// This is also where the shape of the whole change lives. A built-in is a Lua
// file that declares its ports and its settings and calls one host function;
// the host function is the Go implementation that node always had, running on
// the node, the upstream values and the runtime this execution already holds.
// Nothing privileged moved into Lua, and nothing a script says can point a host
// function at data the graph did not give the node.

// unknownNodeType is the error for a type neither the switch nor any installed
// pack has. It names the type, and says why a type can be unknown on one
// machine and fine on another, because the person reading it has usually just
// opened someone else's workflow.
func unknownNodeType(nodeType string) error {
	return fmt.Errorf("unknown node type: %s (if this node comes from a pack, that pack may not be installed in this project)", nodeType)
}

// runPluginNode executes one node contributed by a pack, built-in or installed.
func (r *Runtime) runPluginNode(ctx context.Context, in *RunInput) (*NodeOutput, error) {
	nodeType := in.Node.Type
	reg := r.registry()
	if reg == nil {
		return nil, unknownNodeType(nodeType)
	}
	def, ok := reg.Kind(nodeType)
	if !ok {
		return nil, unknownNodeType(nodeType)
	}

	out, log, err := reg.RunWithLog(ctx, nodeType, r.pluginHostInput(def, in))
	// Recorded before the error is checked: a node that failed after printing
	// three lines is a node whose author needs those three lines, and they are
	// gone the moment this function returns without keeping them.
	r.recordPluginLog(in.Node.ID, log)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("%s returned no output", nodeType)
	}
	// plugins.Output mirrors NodeOutput field for field, and is a separate type
	// only because the import cannot go the other way. Nothing downstream can
	// tell a pack node's result from a built-in's.
	return &NodeOutput{Type: out.Type, Value: out.Value}, nil
}

// pluginHostInput assembles everything one plugin node execution is given.
//
// This is where the decision is made about what a node may reach, and it is
// made from the pack's declared capabilities and nothing else. The sandbox
// checks the same list again before it puts a function on ctx, but a capability
// is worth little if the only thing enforcing it is the code on the far side of
// the boundary: a pack that did not declare a capability must not be handed a
// live function at all, so that a future bug in the sandbox's own gate has
// nothing to leak.
func (r *Runtime) pluginHostInput(def *plugins.NodeDef, in *RunInput) plugins.HostInput {
	host := plugins.HostInput{
		Input:  firstNonNil(in.Upstream),
		Config: r.resolvePluginConfig(in.Config),
		Limits: r.PluginLimits,
	}
	if def.Has(plugins.CapLLM) {
		host.LLM = r.pluginLLM(in.Config)
		host.Complete = r.nodeFunc(in, r.runLLM)
	}
	if def.Has(plugins.CapImage) {
		host.Image = plugins.ImageFuncs{
			Generate:         r.nodeFunc(in, r.runGenerateImage),
			Edit:             r.nodeFunc(in, r.runEditImage),
			RemoveBackground: r.nodeFunc(in, r.runRemoveBackground),
			Rotate:           r.nodeFunc(in, withoutContext(r.runRotateImage)),
			Flip:             r.nodeFunc(in, withoutContext(r.runFlipImage)),
			Compose:          r.nodeFunc(in, withoutContext(r.runVoxelPreview)),
		}
	}
	if def.Has(plugins.CapVision) {
		host.Vision = plugins.VisionFuncs{Describe: r.nodeFunc(in, r.runVision)}
	}
	if def.Has(plugins.CapAgent) {
		host.Agent = plugins.AgentFuncs{
			Brain: r.nodeFunc(in, r.runBrain),
			Tools: r.nodeFunc(in, withoutContext(r.runHostTools)),
		}
	}
	if def.Has(plugins.CapFiles) && r.Files != nil {
		host.Files = r.Files
	}
	return host
}

// nodeRun is the shape of every one of the engine's own node implementations.
type nodeRun func(ctx context.Context, in *RunInput) (*NodeOutput, error)

// withoutContext adapts the implementations that need no context — the ones
// that do arithmetic on pixels rather than talk to anybody.
func withoutContext(run func(in *RunInput) (*NodeOutput, error)) nodeRun {
	return func(_ context.Context, in *RunInput) (*NodeOutput, error) { return run(in) }
}

// nodeFunc turns one of the engine's own node implementations into the host
// function a Lua node calls.
//
// The node, its upstream values and the runtime are the ones this execution
// already has; the script supplies only the settings. That is the division the
// whole design rests on: a Lua node decides what to ask for, and the host
// decides what it is asked about. A script cannot point generateImage at an
// image the graph did not give this node, because the image never crosses the
// boundary in that direction at all.
//
// The config the script passed has already been through the placeholder
// resolver once, on its way in — which is why the implementations below read
// their settings straight rather than resolving them again.
func (r *Runtime) nodeFunc(in *RunInput, run nodeRun) plugins.NodeFunc {
	return func(ctx context.Context, cfg map[string]any) (*plugins.Output, error) {
		if cfg == nil {
			cfg = map[string]any{}
		}
		out, err := run(ctx, &RunInput{Node: in.Node, Upstream: in.Upstream, Config: cfg})
		if err != nil {
			return nil, err
		}
		if out == nil {
			return nil, fmt.Errorf("%s produced no output", in.Node.Type)
		}
		return &plugins.Output{Type: out.Type, Value: out.Value}, nil
	}
}

// firstNonNil is the value a plugin node reads on its input: the first upstream
// output that produced something. A node with several inputs sees the first,
// the way preview and output nodes do; a pack that needs more than one value
// takes a mergeText upstream of it, like the rest of the graph.
func firstNonNil(ups []*NodeOutput) *plugins.Output {
	for _, u := range ups {
		if u != nil {
			return &plugins.Output{Type: u.Type, Value: u.Value}
		}
	}
	return nil
}

// resolvePluginConfig substitutes the {{input:...}} placeholders before the
// config crosses the boundary, so a script never sees the unresolved form.
// Resolving them is the runtime's job: a pack that had to do it itself would be
// a pack that could decide not to.
//
// The result is a new map. The one it reads is the node's own Data, which the
// graph keeps for the rest of the run and the fingerprint has already hashed.
func (r *Runtime) resolvePluginConfig(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = r.resolvePluginValue(v, 0)
	}
	return out
}

// maxConfigDepth bounds the walk. A config comes from JSON the user's own
// builder wrote, but it arrives over HTTP, and a self-describing recursive
// shape is cheap to write and expensive to recurse into.
const maxConfigDepth = 32

func (r *Runtime) resolvePluginValue(v any, depth int) any {
	if depth > maxConfigDepth {
		return nil
	}
	switch t := v.(type) {
	case string:
		return r.resolveInputs(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = r.resolvePluginValue(item, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = r.resolvePluginValue(item, depth+1)
		}
		return out
	default:
		return v
	}
}

// pluginLLM is the one model call a node may make, bound to this runtime's
// providers. The pack names a model at most; provider selection, credentials
// and routing stay on this side, which is what keeps a pack from being able to
// name an endpoint.
func (r *Runtime) pluginLLM(cfg map[string]any) plugins.LLMFunc {
	// Read once, outside the closure, for the same reason runLLM reads them at
	// the top: these are the node's settings for this execution, and a node
	// looping over ctx.llm must not be able to see them change underneath it.
	provider := str(cfg["provider"])
	model := str(cfg["model"])
	temperature := num(cfg["temperature"], 0.7)

	return func(ctx context.Context, req plugins.LLMRequest) (string, error) {
		// Failing rather than being absent when no provider is configured.
		// Which functions ctx carries has to depend on the pack's declared
		// capabilities and on nothing else: a function that disappeared when
		// the host happened to have no provider would let a pack ask about the
		// machine it is running on, and a pack that can tell it is not being
		// watched is a pack that can behave when it is.
		if r.Providers == nil {
			return "", fmt.Errorf("no model provider is configured for this run")
		}
		messages := []providers.Message{}
		if req.System != "" {
			messages = append(messages, providers.Message{Role: "system", Content: req.System})
		}
		messages = append(messages, providers.Message{Role: "user", Content: req.Prompt})

		// The node's config wins over the model the script asked for. The
		// config is the user's choice in the builder and the script's is the
		// pack author's suggestion; the account being spent is the user's.
		chosen := model
		if chosen == "" {
			chosen = req.Model
		}

		resp, err := r.Providers.LLMComplete(ctx, providers.LLMRequest{
			Messages: messages,
			// Empty provider falls back to the deployment default, exactly as
			// it does for a built-in llm node, so a pack node can pin a
			// provider the same way a built-in can.
			Provider:    provider,
			Model:       chosen,
			MaxTokens:   req.MaxTokens,
			Temperature: temperature,
		})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

// recordPluginLog keeps what a node printed, keyed by node id. It appends
// rather than replaces because the Brain can invoke the same node more than
// once in a run, and the second call's log is not a correction of the first.
func (r *Runtime) recordPluginLog(nodeID string, lines []string) {
	if len(lines) == 0 {
		return
	}
	if r.PluginLogs == nil {
		r.PluginLogs = map[string][]string{}
	}
	r.PluginLogs[nodeID] = append(r.PluginLogs[nodeID], lines...)
}
