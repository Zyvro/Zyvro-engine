package engine

import (
	"context"
	"fmt"

	"github.com/Zyvro/Zyvro-engine/plugins"
	"github.com/Zyvro/Zyvro-engine/providers"
)

// The bridge between the graph and a pack's Lua nodes.
//
// The engine imports plugins and never the other way round, so everything that
// has to cross — the upstream value, the resolved config, the two capabilities
// — is assembled here and handed over as plain data. What a node may reach is
// decided on this side: the sandbox enforces the budget, but the decision to
// lend it the user's model account or their project folder is the runtime's,
// and it is made once, in pluginHostInput.

// unknownNodeType is the error for a type neither the switch nor any installed
// pack has. It names the type, and says why a type can be unknown on one
// machine and fine on another, because the person reading it has usually just
// opened someone else's workflow.
func unknownNodeType(nodeType string) error {
	return fmt.Errorf("unknown node type: %s (if this node comes from a pack, that pack may not be installed in this project)", nodeType)
}

// runPluginNode executes one node contributed by a pack.
func (r *Runtime) runPluginNode(ctx context.Context, in *RunInput) (*NodeOutput, error) {
	nodeType := in.Node.Type
	if r.Plugins == nil {
		return nil, unknownNodeType(nodeType)
	}
	def, ok := r.Plugins.Kind(nodeType)
	if !ok {
		return nil, unknownNodeType(nodeType)
	}

	out, log, err := r.Plugins.RunWithLog(ctx, nodeType, r.pluginHostInput(def, in))
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
func (r *Runtime) pluginHostInput(def *plugins.NodeDef, in *RunInput) plugins.HostInput {
	host := plugins.HostInput{
		Input:  firstNonNil(in.Upstream),
		Config: r.resolvePluginConfig(in.Config),
		Limits: r.PluginLimits,
	}
	if def.Has(plugins.CapLLM) {
		host.LLM = r.pluginLLM(in.Config)
	}
	// Files is granted only to a pack whose manifest asked for it. The sandbox
	// checks the same thing before it exposes ctx.readFile, but a capability is
	// worth little if the only thing enforcing it is the code on the far side
	// of the boundary: a pack that did not declare files must not be handed a
	// live FileAccess at all, so that a future bug in the sandbox's own gate
	// has nothing to leak.
	if def.Has(plugins.CapFiles) && r.Files != nil {
		host.Files = r.Files
	}
	return host
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
	if r.Providers == nil {
		// Nil rather than a function that fails: the sandbox leaves ctx.llm off
		// entirely when there is no LLM, and a node that can ask whether it is
		// being watched is a node that can behave when it is.
		return nil
	}
	// Read once, outside the closure, for the same reason runLLM reads them at
	// the top: these are the node's settings for this execution, and a node
	// looping over ctx.llm must not be able to see them change underneath it.
	provider := str(cfg["provider"])
	model := str(cfg["model"])
	temperature := num(cfg["temperature"], 0.7)

	return func(ctx context.Context, req plugins.LLMRequest) (string, error) {
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
