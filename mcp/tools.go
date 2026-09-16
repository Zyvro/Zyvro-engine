package mcp

// The seven tool names. A client that was configured against one host and then
// pointed at the other has these memorised, so they are constants used both to
// build the catalogue and to dispatch a call: a name can then only be wrong in
// one place, and it would not compile there.
const (
	ToolListWorkflows   = "zyvro_list_workflows"
	ToolWorkflowGraph   = "zyvro_workflow_graph"
	ToolRunWorkflow     = "zyvro_run_workflow"
	ToolExecutionStatus = "zyvro_execution_status"
	ToolGetOutput       = "zyvro_get_output"
	ToolListExecutions  = "zyvro_list_executions"
	ToolCancelExecution = "zyvro_cancel_execution"
)

// The limits the tool surface declares or implies. MaxWaitSeconds is written
// into the wait_seconds schema below and enforced by WaitArg, so the two cannot
// disagree; the image caps are not on the wire, but a host that inlined more
// than another would answer the same zyvro_get_output call differently, which
// is the kind of difference this package exists to prevent.
const (
	MaxWaitSeconds = 300

	// Inlining caps for zyvro_get_output: anything larger is reported as a URL.
	MaxImageBytes      = 4 << 20
	MaxTotalImageBytes = 8 << 20
	DefaultMaxImages   = 4
)

// Content is one block of a tool result: text, or an inlined image.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// ToolAnnotations are behaviour hints clients use to decide what to
// auto-approve. readOnlyHint and openWorldHint are always emitted: their
// spec defaults (false, true) are wrong for most tools here.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

type Tool struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema map[string]any   `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// TextResult adapts the string-returning tool helpers both hosts are built
// from to the content blocks the protocol carries.
func TextResult(s string, err error) ([]Content, error) {
	if err != nil {
		return nil, err
	}
	return []Content{{Type: "text", Text: s}}, nil
}

func boolPtr(b bool) *bool { return &b }

func waitSecondsSchema(what string) map[string]any {
	return map[string]any{
		"type":        "integer",
		"minimum":     0,
		"maximum":     MaxWaitSeconds,
		"description": what,
	}
}

// Tools returns the catalogue both hosts advertise, freshly built so that a
// caller which edits a schema map cannot reach the next caller's copy.
//
// descriptions replaces the description of the tools it names, and is the only
// thing a host may vary. A description is prose the model reads, so a sentence
// that is true of the hosted server can be a lie on a laptop — there is no
// queue to be placed in, and nothing that can stop a run once it has started —
// and saying so is better than describing behaviour the host does not have.
// Names, schemas and annotations are not negotiable: those are what a client
// validates arguments against before it ever sends them.
func Tools(descriptions map[string]string) []Tool {
	tools := []Tool{
		{
			Name:        ToolListWorkflows,
			Description: "List the user's Zyvro visual AI workflows (id, name, node count, last update).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			Annotations: &ToolAnnotations{Title: "List workflows", ReadOnlyHint: true},
		},
		{
			Name:        ToolWorkflowGraph,
			Description: "Get a workflow's node graph as a compact outline (node type, label, connections) and the names of its runtime inputs.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{"type": "string", "description": "Workflow ID"},
				},
				"required": []string{"workflow_id"},
			},
			Annotations: &ToolAnnotations{Title: "Inspect workflow", ReadOnlyHint: true},
		},
		{
			Name: ToolRunWorkflow,
			Description: "Run a Zyvro workflow. Call zyvro_workflow_graph first to see the input names, their type (text/image) and whether they are required. " +
				"Pass wait_seconds to get the finished result in this same call; without it the tool returns an execution id to poll with zyvro_execution_status.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{"type": "string", "description": "Workflow ID"},
					"inputs": map[string]any{
						"type": "object", "additionalProperties": true,
						"description": "Map of input name -> value, using the names from zyvro_workflow_graph. Text inputs take a string. Image inputs take a data URL (data:image/png;base64,...) or an http(s) URL.",
					},
					"wait_seconds": waitSecondsSchema("Seconds to wait for the run to finish before returning (0-300, default 0). Image workflows usually finish in 10-60s."),
				},
				"required": []string{"workflow_id"},
			},
			Annotations: &ToolAnnotations{Title: "Run workflow", OpenWorldHint: true},
		},
		{
			Name:        ToolExecutionStatus,
			Description: "Get an execution's status, per-node statuses and outputs (text, absolute media URLs). Pass wait_seconds to block until the run finishes instead of polling in a loop.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"execution_id": map[string]any{"type": "string", "description": "Execution ID"},
					"wait_seconds": waitSecondsSchema("Seconds to wait for a terminal status before returning (0-300, default 0 = answer immediately)."),
				},
				"required": []string{"execution_id"},
			},
			Annotations: &ToolAnnotations{Title: "Execution status", ReadOnlyHint: true},
		},
		{
			Name:        ToolGetOutput,
			Description: "Return the images produced by a finished execution as viewable image content, not just URLs. Use it after a run to actually look at the result.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"execution_id": map[string]any{"type": "string", "description": "Execution ID"},
					"node_id":      map[string]any{"type": "string", "description": "Optional: only this node's images. Default: the output/preview nodes of the run."},
					"max_images":   map[string]any{"type": "integer", "minimum": 1, "maximum": 8, "description": "Maximum images to inline (default 4)."},
				},
				"required": []string{"execution_id"},
			},
			Annotations: &ToolAnnotations{Title: "Get output images", ReadOnlyHint: true},
		},
		{
			Name:        ToolListExecutions,
			Description: "List recent executions, newest first, with their status and duration. Optionally scoped to one workflow.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workflow_id": map[string]any{"type": "string", "description": "Optional: only this workflow's executions."},
					"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "How many to return (default 10)."},
				},
			},
			Annotations: &ToolAnnotations{Title: "List executions", ReadOnlyHint: true},
		},
		{
			Name:        ToolCancelExecution,
			Description: "Stop a queued or running execution. In-flight provider calls are aborted; finished executions are left untouched.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"execution_id": map[string]any{"type": "string", "description": "Execution ID"},
				},
				"required": []string{"execution_id"},
			},
			Annotations: &ToolAnnotations{
				Title:           "Cancel execution",
				DestructiveHint: boolPtr(false),
				IdempotentHint:  boolPtr(true),
			},
		},
	}
	for i := range tools {
		if d, ok := descriptions[tools[i].Name]; ok {
			tools[i].Description = d
		}
	}
	return tools
}
