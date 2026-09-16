package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Zyvro/Zyvro-engine/providers"
)

// Brain node: an agent that receives a goal and calls connected tool nodes
// (linked via "tool" edges) until it produces a final answer.
//
// Tool links are NOT DAG dependencies: the Brain chooses which tools to run
// at runtime. Tool nodes execute through ExecuteNode so a tool can be an
// arbitrary node type (generateImage, removeBackground, vision, llm...).

const brainMaxStepsDefault = 6

// brainMaxStepsCeiling bounds one Brain run however many steps it was asked for.
//
// It exists because of the agent capability. A Brain's step count used to come
// only from a node's config, which a person set in the builder and could see;
// it now also arrives from a Lua node, where nobody sees it, and one ctx.brain
// call costs one unit of the model budget no matter how many turns it takes. A
// pack asking for a million steps would be asking for a million model calls
// against a budget of eight, and the run deadline is the only other thing in its
// way. Fifty is far past what any real agent needs and far short of a bill.
const brainMaxStepsCeiling = 50

// runBrain is the Go behind ctx.brain, the agent capability's one function. It
// reads the settings the bundled pack's brain.lua passed, which the bridge has
// already run the placeholder resolver over; the goal arriving on an input has
// not been, so that one still is.
func (r *Runtime) runBrain(ctx context.Context, in *RunInput) (*NodeOutput, error) {
	goal := str(in.Config["goal"])
	if goal == "" {
		if t := firstUpstream(in.Upstream, "text"); t != nil {
			goal = r.resolveInputs(str(t.Value["text"]))
		}
	}
	if goal == "" {
		return nil, fmt.Errorf("brain node needs a goal")
	}
	system := strDefault(in.Config["system"], "You are a visual AI agent. Use the available tools to accomplish the goal. When the goal is achieved, give your final answer describing the result.")
	maxSteps := int(num(in.Config["maxSteps"], brainMaxStepsDefault))
	if maxSteps > brainMaxStepsCeiling {
		maxSteps = brainMaxStepsCeiling
	}

	toolNodeIDs := ToolNodesOf(r.Graph, in.Node.ID)
	if len(toolNodeIDs) == 0 {
		return nil, fmt.Errorf("brain node has no tools connected")
	}

	// Build tool schemas from connected nodes. Each tool's arguments come
	// from the upstream inputs the tool node can accept, prefixed by a
	// mandatory "instructions" prompt.
	type toolBinding struct {
		nodeID   string
		node     *GraphNode
		schema   providers.ToolSchema
		external bool
	}
	bindings := map[string]toolBinding{}
	var tools []providers.Tool
	for _, tid := range toolNodeIDs {
		tn := NodeByID(r.Graph, tid)
		if tn == nil {
			continue
		}
		if tn.Type == "zyvroTools" {
			// Host-provided tools (MCP surface): one function per schema.
			if r.External == nil {
				continue
			}
			for _, schema := range r.External.Schemas() {
				bindings[schema.Name] = toolBinding{nodeID: tid, node: tn, schema: schema, external: true}
				tools = append(tools, providers.Tool{Type: "function", Function: schema})
			}
			continue
		}
		name := toolName(tn)
		properties := map[string]any{
			"instructions": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("Instructions or prompt to pass to the %s capability.", tn.Type),
			},
		}
		schema := providers.ToolSchema{
			Name:        name,
			Description: fmt.Sprintf("Execute the %s capability (%s) connected to this agent.", tn.Type, name),
			Parameters: map[string]any{
				"type":       "object",
				"properties": properties,
				"required":   []string{"instructions"},
			},
		}
		bindings[name] = toolBinding{nodeID: tid, node: tn, schema: schema}
		tools = append(tools, providers.Tool{Type: "function", Function: schema})
	}

	messages := []providers.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: "Goal: " + goal},
	}

	if len(tools) == 0 {
		return nil, fmt.Errorf("brain node has no usable tools")
	}

	var trace []AgentStep
	var sessionImage *NodeOutput
	var finalAnswer string
	var images []string

	for step := 1; step <= maxSteps; step++ {
		resp, err := r.Providers.LLMComplete(ctx, providers.LLMRequest{
			Messages:    messages,
			Tools:       tools,
			Provider:    str(in.Config["provider"]),
			Model:       str(in.Config["model"]),
			MaxTokens:   2000,
			Temperature: 0.4,
		})
		if err != nil {
			return nil, fmt.Errorf("brain LLM call failed at step %d: %w", step, err)
		}

		// Final answer without tool call -> done.
		if len(resp.ToolCalls) == 0 {
			finalAnswer = resp.Content
			if strings.TrimSpace(finalAnswer) == "" {
				finalAnswer = "Agent finished."
			}
			trace = append(trace, AgentStep{Step: step, Tool: "final_answer", Summary: firstLine(finalAnswer), Status: "success"})
			break
		}

		assistantMsg := providers.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		for _, call := range resp.ToolCalls {
			binding, ok := bindings[call.Function.Name]
			if !ok {
				messages = append(messages, providers.Message{
					Role: "tool", ToolCallID: call.ID,
					Content: "error: unknown tool " + call.Function.Name,
				})
				trace = append(trace, AgentStep{Step: step, Tool: call.Function.Name, Summary: "unknown tool", Status: "error"})
				continue
			}

			var args map[string]any
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				args = map[string]any{}
			}

			if binding.external {
				started := time.Now()
				text, imgs, execID, err := r.External.Call(ctx, call.Function.Name, args)
				st := AgentStep{Step: step, Tool: call.Function.Name, Args: truncateStr(compactArgs(args), 300), ExecutionID: execID, DurationMs: time.Since(started).Milliseconds()}
				if err != nil {
					text = "error: " + err.Error()
					st.Status, st.Summary, st.Result = "error", firstLine(err.Error()), truncateStr(text, 400)
				} else {
					st.Status, st.Summary, st.Result = "success", firstLine(text), truncateStr(text, 400)
				}
				images = append(images, imgs...)
				trace = append(trace, st)
				messages = append(messages, providers.Message{Role: "tool", ToolCallID: call.ID, Content: text})
				continue
			}

			instructions := fmt.Sprint(args["instructions"])

			toolOut, err := r.executeTool(ctx, binding.node, instructions, sessionImage)
			if err != nil {
				messages = append(messages, providers.Message{
					Role: "tool", ToolCallID: call.ID,
					Content: "error: " + err.Error(),
				})
				trace = append(trace, AgentStep{Step: step, Tool: binding.node.Type, Summary: firstLine(err.Error()), Status: "error"})
				continue
			}

			observation := observeOutput(toolOut)
			if toolOut.Type == "image" {
				sessionImage = toolOut
			}
			messages = append(messages, providers.Message{
				Role: "tool", ToolCallID: call.ID,
				Content: observation,
			})
			trace = append(trace, AgentStep{Step: step, Tool: binding.node.Type, Summary: truncateStr(instructions, 80), Status: "success"})
		}
	}

	if finalAnswer == "" {
		finalAnswer = "Agent stopped after reaching the maximum number of steps."
	}

	r.AgentTrace = append(r.AgentTrace, trace...)

	out := &NodeOutput{Type: "text", Value: map[string]any{
		"text":  finalAnswer,
		"trace": trace,
	}}
	if len(images) > 0 {
		out.Value["images"] = images
		out.Value["url"] = images[len(images)-1]
	}
	if sessionImage != nil {
		if u, ok := sessionImage.Value["url"].(string); ok && u != "" {
			out.Value["url"] = u
		}
		out.Value["imageDataUrl"] = sessionImage.Value["dataUrl"]
	}
	return out, nil
}

// executeTool runs a tool node for the Brain. Tool nodes are isolated from
// the data flow (their only edges are tool edges), so the Brain provides the
// inputs directly: instructions become the prompt text, and the most recent
// image produced during this agent run becomes the image input when the tool
// needs one (editImage, removeBackground, vision).
func (r *Runtime) executeTool(ctx context.Context, node *GraphNode, instructions string, sessionImage *NodeOutput) (*NodeOutput, error) {
	var ups []*NodeOutput
	if sessionImage != nil {
		switch node.Type {
		case "vision", "editImage", "removeBackground":
			ups = []*NodeOutput{sessionImage}
		case "generateImage":
			// Upstream images act as generation references in the DAG; give
			// the Brain the same ability on its current image.
			ups = []*NodeOutput{sessionImage}
		}
	}
	if needsTextPrompt(node.Type) {
		ups = append(ups, textOutput(instructions))
	}

	cfg := map[string]any{}
	for k, v := range nodeConfig(node) {
		cfg[k] = v
	}
	// Explicit node config wins over injected instructions for the prompt
	// slot, matching how the node would behave in the DAG.
	if str(cfg["prompt"]) == "" && needsTextPrompt(node.Type) {
		cfg["prompt"] = instructions
	}
	if node.Type == "llm" && str(cfg["prompt"]) == "" {
		cfg["prompt"] = instructions
	}
	if node.Type == "vision" && str(cfg["instruction"]) == "" {
		cfg["instruction"] = instructions
	}

	return r.executeWithInput(ctx, &RunInput{
		Node:     node,
		Upstream: ups,
		Config:   cfg,
	})
}

func needsTextPrompt(nodeType string) bool {
	switch nodeType {
	case "generateImage", "editImage", "llm":
		return true
	}
	return false
}

func observeOutput(o *NodeOutput) string {
	switch o.Type {
	case "text":
		return truncateStr(str(o.Value["text"]), 1500)
	case "image":
		meta := fmt.Sprintf("image generated (%s, %d bytes)", str(o.Value["mimeType"]), int(num(o.Value["bytes"], 0)))
		if u, ok := o.Value["url"].(string); ok && u != "" {
			return meta + ", stored at " + u
		}
		return meta
	case "json":
		b, _ := json.Marshal(o.Value["data"])
		return "json: " + truncateStr(string(b), 800)
	}
	b, _ := json.Marshal(o.Value)
	return truncateStr(string(b), 800)
}

func toolName(n *GraphNode) string {
	base := n.Type
	if base == "" {
		base = "tool"
	}
	if l := str(n.Data["label"]); l != "" && l != base {
		slug := strings.ToLower(strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				return r
			default:
				return '_'
			}
		}, l))
		if slug != "" && len(slug) <= 40 {
			return base + "_" + strings.Trim(slug, "_")
		}
	}
	return base + "_" + lastSeg(n.ID)
}

func lastSeg(id string) string {
	if i := strings.LastIndex(id, "-"); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

func compactArgs(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
