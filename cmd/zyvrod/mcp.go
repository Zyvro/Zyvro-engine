package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zyvro/Zyvro-engine/engine"
	"github.com/Zyvro/Zyvro-engine/localstore"
	"github.com/Zyvro/Zyvro-engine/mcp"
)

// The daemon's MCP (Model Context Protocol) server over HTTP JSON-RPC 2.0.
// It is the same tool surface as the hosted server, backed by the opened
// project folder instead of a database, so the agent in Zyvro Studio's chat
// panel can actually drive the project's workflows rather than only be told
// that they exist. A client that learned the hosted tools finds the same names
// and the same schemas here, because both servers take them from the mcp
// package rather than each keeping a copy.
//
// Auth: the daemon's own bearer token, the one handed to the parent process in
// the startup handshake. /mcp sits behind the same middleware as /api, because
// a second credential for the same process would only be a second thing to
// leak.
//
// Supported methods:
//   initialize    -> server info + capabilities (protocol version negotiated)
//   tools/list    -> the Zyvro tools exposed to AI clients
//   tools/call    -> execute a tool (list/run workflows, poll executions)

// mcpProtocolVersions is the shared list under the name this package knows it
// by. The slice belongs to mcp; read it, never write through it.
var mcpProtocolVersions = mcp.ProtocolVersions

// mcpContent is the shared content block under a local name, because the tool
// results this file builds and the tests that read them both spell it this way.
type mcpContent = mcp.Content

const (
	mcpPollInterval = 250 * time.Millisecond

	// mcpMaxBodyBytes bounds one JSON-RPC message. Image inputs arrive as data
	// URLs inside tools/call, so the limit has to be generous enough to carry a
	// photograph and no more.
	mcpMaxBodyBytes = 32 << 20
)

const mcpInstructions = `Zyvro runs visual AI workflows (graphs of text/image/vision nodes). This server is the
local daemon: the workflows are the ones stored in the project folder currently open in Zyvro Studio.
Typical flow: zyvro_list_workflows -> zyvro_workflow_graph (to learn the input names) -> zyvro_run_workflow.
Pass wait_seconds to zyvro_run_workflow to get the result in one call instead of polling.
Use zyvro_get_output to see the generated images themselves rather than their URLs.`

// mcpServer is the daemon's half of the shared server. It takes the catalogue
// name for name and schema for schema; only one description is reworded, and
// only because the hosted sentence would be a lie here: there is nothing on a
// laptop that can stop a run once it has started, and a tool that claimed
// otherwise would leave the user believing they had stopped spending money.
var mcpServer = &mcp.Server{
	Name:         "zyvro",
	Version:      "0.2.0",
	Instructions: mcpInstructions,
	Catalogue: mcp.Tools(map[string]string{
		mcp.ToolCancelExecution: "Stop a running execution. The local daemon runs workflows in-process with no way to interrupt one, so this tool reports that it could not cancel " +
			"rather than pretending it did; a run stops on its own when it finishes or times out.",
	}),
	MaxBodyBytes: mcpMaxBodyBytes,
	OnWriteError: func(err error) { log.Printf("write response: %v", err) },
}

// mcpHandler is the single MCP endpoint (Streamable HTTP transport, JSON mode).
// It is mounted behind requireToken, so by the time it runs the caller has
// already proved it holds the handshake token.
func (d *daemon) mcpHandler(w http.ResponseWriter, r *http.Request) {
	// OPTIONS never arrives here — the cors wrapper answers every preflight
	// before the route table is consulted.
	if r.Method != http.MethodPost {
		// The server does not open SSE streams, so GET/DELETE are unsupported
		// as the Streamable HTTP transport allows.
		writeErr(w, http.StatusMethodNotAllowed, "MCP endpoint accepts POST only")
		return
	}
	mcpServer.Serve(w, r, &localTools{d: d, origin: mcpOrigin(r)})
}

// localTools runs the catalogue against the open project folder. It is built
// per request because the media URLs it hands back have to be absolute, and
// the only honest source for the origin is the request that just arrived.
type localTools struct {
	d      *daemon
	origin string
}

func (l *localTools) CallTool(ctx context.Context, name string, args map[string]any) ([]mcpContent, error) {
	d := l.d
	switch name {
	case mcp.ToolListWorkflows:
		return mcp.TextResult(d.mcpListWorkflows())
	case mcp.ToolWorkflowGraph:
		return mcp.TextResult(d.mcpWorkflowGraph(mcp.StrArg(args, "workflow_id")))
	case mcp.ToolRunWorkflow:
		return mcp.TextResult(d.mcpRunWorkflow(ctx, args, l.origin))
	case mcp.ToolExecutionStatus:
		return mcp.TextResult(d.mcpExecutionStatusWait(ctx, mcp.StrArg(args, "execution_id"), mcp.WaitArg(args), l.origin))
	case mcp.ToolGetOutput:
		return d.mcpGetOutput(args, l.origin)
	case mcp.ToolListExecutions:
		return mcp.TextResult(d.mcpListExecutions(args))
	case mcp.ToolCancelExecution:
		return mcp.TextResult(d.mcpCancelExecution(mcp.StrArg(args, "execution_id")))
	}
	return nil, mcp.ErrUnknownTool
}

func (d *daemon) mcpListWorkflows() (string, error) {
	wfs, err := d.store.List()
	if err != nil {
		return "", fmt.Errorf("failed to list workflows: %w", err)
	}
	if len(wfs) == 0 {
		// The hosted server links to the dashboard here. There is no dashboard
		// URL on a laptop, so the answer names the folder the user opened,
		// which is the thing they would actually go and look at.
		return "No workflows found in " + d.store.Root + ". Create one in Zyvro Studio.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d workflow(s):\n", len(wfs))
	for _, wf := range wfs {
		nodes := 0
		if g, err := engine.ParseGraph(wf.GraphJSON); err == nil {
			nodes = len(g.Nodes)
		}
		fmt.Fprintf(&b, "- %s | id=%s | %d nodes | updated %s\n", wf.Name, wf.ID, nodes, wf.UpdatedAt.Format("2006-01-02"))
	}
	return b.String(), nil
}

func (d *daemon) mcpWorkflowGraph(wfID string) (string, error) {
	if wfID == "" {
		return "", fmt.Errorf("workflow_id required")
	}
	wf, err := d.store.Get(wfID)
	if err != nil {
		return "", fmt.Errorf("workflow not found")
	}
	g, err := engine.ParseGraph(wf.GraphJSON)
	if err != nil {
		return "", fmt.Errorf("invalid graph: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow: %s (id=%s)\n", wf.Name, wf.ID)
	if desc := strings.TrimSpace(wf.Description); desc != "" {
		fmt.Fprintf(&b, "Description: %s\n", mcpTruncate(desc, 200))
	}
	// Named runtime inputs are the public contract of the workflow.
	var inputs []string
	for _, n := range g.Nodes {
		if n.Type != "textInput" && n.Type != "imageInput" {
			continue
		}
		key := engine.InputKey(&n)
		if key == "" {
			continue
		}
		kind := "text"
		def := ""
		cfg, _ := n.Data["config"].(map[string]any)
		if n.Type == "imageInput" {
			kind = "image"
			if cfg != nil {
				if v, _ := cfg["dataUrl"].(string); strings.TrimSpace(v) != "" {
					def = "has a default image"
				}
			}
		} else if cfg != nil {
			if v, _ := cfg["value"].(string); strings.TrimSpace(v) != "" {
				def = "default=" + mcpTruncate(v, 40)
			}
		}
		req := "required"
		if def != "" {
			req = "optional, " + def
		}
		label, _ := n.Data["label"].(string)
		inputs = append(inputs, fmt.Sprintf("- %s (%s, %s) node=%s %s", key, kind, req, n.ID, label))
	}
	if len(inputs) > 0 {
		fmt.Fprintf(&b, "Runtime inputs (pass as zyvro_run_workflow.inputs):\n%s\n", strings.Join(inputs, "\n"))
	} else {
		fmt.Fprintf(&b, "Runtime inputs: none declared (input nodes can still be overridden by node id).\n")
	}
	fmt.Fprintf(&b, "Nodes:\n")
	for _, n := range g.Nodes {
		label, _ := n.Data["label"].(string)
		cfg, _ := n.Data["config"].(map[string]any)
		hint := ""
		if cfg != nil {
			if p, _ := cfg["prompt"].(string); p != "" {
				hint = " prompt=" + mcpTruncate(p, 60)
			} else if v, _ := cfg["value"].(string); v != "" {
				hint = " value=" + mcpTruncate(v, 40)
			} else if goal, _ := cfg["goal"].(string); goal != "" {
				hint = " goal=" + mcpTruncate(goal, 60)
			}
		}
		fmt.Fprintf(&b, "- %s [%s] %s%s\n", n.ID, n.Type, label, hint)
	}
	if len(g.Edges) > 0 {
		fmt.Fprintf(&b, "Edges:\n")
		for _, e := range g.Edges {
			kind := e.Type
			if kind == "" {
				kind = "data"
			}
			fmt.Fprintf(&b, "- %s -> %s (%s)\n", e.Source, e.Target, kind)
		}
	}
	fmt.Fprintf(&b, "Run: zyvro_run_workflow {workflow_id, inputs: {<input name>: <string | image data URL | image http URL>}, wait_seconds}.")
	return b.String(), nil
}

// mcpRunWorkflow starts a run through the daemon's one run entry point, the
// same one POST /api/executions uses, so a workflow started by the agent and
// one started from the builder are the same thing to the rest of the daemon.
func (d *daemon) mcpRunWorkflow(ctx context.Context, args map[string]any, origin string) (string, error) {
	var inputs map[string]any
	if raw, ok := args["inputs"]; ok {
		if m, ok := raw.(map[string]any); ok {
			inputs = m
		}
	}
	run, err := d.startRun(runRequest{WorkflowID: mcp.StrArg(args, "workflow_id"), Inputs: inputs})
	if err != nil {
		return "", err
	}
	execID := run.ID

	wait := mcp.WaitArg(args)
	if wait <= 0 {
		// The hosted answer reports a queue position here. There is no queue on
		// a laptop — the run has already started — so the line is left out
		// rather than filled with a zero that would read as "first in line".
		return fmt.Sprintf(
			"Execution started.\nexecution_id=%s\nPoll it with zyvro_execution_status (typically 10-60s for image workflows), "+
				"or pass wait_seconds next time to get the result in one call.",
			execID,
		), nil
	}

	exec, werr := d.waitForTerminal(ctx, execID, wait)
	if werr != nil || exec == nil {
		return fmt.Sprintf("Execution started.\nexecution_id=%s\nCould not read its status back; poll with zyvro_execution_status.", execID), nil
	}
	if !isTerminalStatus(exec.Status) {
		return fmt.Sprintf(
			"Execution %s is still %s after waiting %s.\nPoll it with zyvro_execution_status (it accepts wait_seconds too).",
			execID, exec.Status, wait,
		), nil
	}
	return d.mcpExecutionStatus(execID, origin)
}

// mcpExecutionStatusWait blocks until the execution reaches a terminal state
// or wait elapses, then reports it. wait <= 0 answers immediately.
func (d *daemon) mcpExecutionStatusWait(ctx context.Context, execID string, wait time.Duration, origin string) (string, error) {
	if execID == "" {
		return "", fmt.Errorf("execution_id required")
	}
	if wait > 0 {
		if _, err := d.store.GetRun(execID); err != nil {
			return "", fmt.Errorf("execution not found")
		}
		if _, err := d.waitForTerminal(ctx, execID, wait); err != nil {
			return "", fmt.Errorf("execution not found")
		}
	}
	return d.mcpExecutionStatus(execID, origin)
}

func (d *daemon) mcpExecutionStatus(execID, origin string) (string, error) {
	if execID == "" {
		return "", fmt.Errorf("execution_id required")
	}
	run, err := d.store.GetRun(execID)
	if err != nil {
		return "", fmt.Errorf("execution not found")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Execution %s: %s\n", run.ID, run.Status)
	if run.Error != "" {
		fmt.Fprintf(&b, "Error: %s\n", run.Error)
	}
	for _, n := range run.Nodes {
		line := fmt.Sprintf("- %s [%s]: %s", n.NodeID, n.NodeType, n.Status)
		if n.LatencyMs > 0 {
			line += fmt.Sprintf(" (%dms)", n.LatencyMs)
		}
		if n.Error != "" {
			line += " error=" + mcpTruncate(n.Error, 120)
		} else if n.OutputJSON != "" {
			line += " " + describeNodeOutput(n.OutputJSON, origin)
		}
		fmt.Fprintf(&b, "%s\n", line)
	}
	if isTerminalStatus(run.Status) && hasImageOutput(run.Nodes) {
		fmt.Fprintf(&b, "Call zyvro_get_output {execution_id: %q} to view the image(s).\n", run.ID)
	}
	return b.String(), nil
}

// mcpGetOutput returns the run's images as image content blocks so the client
// can actually see them, with a text block listing their absolute URLs.
func (d *daemon) mcpGetOutput(args map[string]any, origin string) ([]mcpContent, error) {
	execID := mcp.StrArg(args, "execution_id")
	if execID == "" {
		return nil, fmt.Errorf("execution_id required")
	}
	run, err := d.store.GetRun(execID)
	if err != nil {
		return nil, fmt.Errorf("execution not found")
	}
	if !isTerminalStatus(run.Status) {
		return nil, fmt.Errorf("execution is %s; wait for it to finish (zyvro_execution_status accepts wait_seconds)", run.Status)
	}

	urls := executionImageURLs(run, mcp.StrArg(args, "node_id"))
	if len(urls) == 0 {
		return []mcpContent{{Type: "text", Text: fmt.Sprintf("Execution %s (%s) produced no image output.", execID, run.Status)}}, nil
	}

	maxImages := mcp.DefaultMaxImages
	if n := mcp.IntArg(args, "max_images"); n > 0 {
		maxImages = n
	}
	truncated := 0
	if len(urls) > maxImages {
		truncated = len(urls) - maxImages
		urls = urls[:maxImages]
	}

	var images []mcpContent
	var lines []string
	total := 0
	for _, u := range urls {
		abs := absoluteMediaURL(origin, u)
		name, ok := mediaFilename(u)
		if !ok {
			lines = append(lines, "- "+abs+" (not stored locally, fetch the URL)")
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(d.store.MediaDir(), name))
		if rerr != nil {
			lines = append(lines, "- "+abs+" (missing from the project's media directory)")
			continue
		}
		if len(data) > mcp.MaxImageBytes || total+len(data) > mcp.MaxTotalImageBytes {
			lines = append(lines, fmt.Sprintf("- %s (%d KB, too large to inline — fetch the URL)", abs, len(data)/1024))
			continue
		}
		total += len(data)
		images = append(images, mcpContent{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(data),
			MimeType: mimeForFile(name),
		})
		lines = append(lines, "- "+abs)
	}

	header := fmt.Sprintf("Execution %s (%s): %d image(s).", execID, run.Status, len(urls))
	if truncated > 0 {
		header += fmt.Sprintf(" %d more not shown (raise max_images).", truncated)
	}
	return append([]mcpContent{{Type: "text", Text: header + "\n" + strings.Join(lines, "\n")}}, images...), nil
}

// executionImageURLs collects the image URLs a run produced, across every node
// or only the one the caller named.
func executionImageURLs(run *localstore.Run, nodeID string) []string {
	var out []string
	for _, n := range run.Nodes {
		if nodeID != "" && n.NodeID != nodeID {
			continue
		}
		if n.OutputJSON == "" {
			continue
		}
		var o struct {
			Type  string         `json:"type"`
			Value map[string]any `json:"value"`
		}
		if json.Unmarshal([]byte(n.OutputJSON), &o) != nil {
			continue
		}
		if imgs, ok := o.Value["images"].([]any); ok {
			for _, v := range imgs {
				if s, ok := v.(string); ok && s != "" {
					out = append(out, s)
				}
			}
		}
		if o.Type == "image" {
			if u, _ := o.Value["url"].(string); u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// hasImageOutput reports whether any node of the run produced an image, so the
// status text can point at zyvro_get_output only when there is something to see.
func hasImageOutput(nodes []localstore.NodeExecution) bool {
	for _, n := range nodes {
		if n.OutputJSON == "" {
			continue
		}
		var o struct {
			Type  string         `json:"type"`
			Value map[string]any `json:"value"`
		}
		if json.Unmarshal([]byte(n.OutputJSON), &o) != nil {
			continue
		}
		if o.Type == "image" {
			return true
		}
		if imgs, ok := o.Value["images"].([]any); ok && len(imgs) > 0 {
			return true
		}
	}
	return false
}

func (d *daemon) mcpListExecutions(args map[string]any) (string, error) {
	limit := mcp.IntArg(args, "limit")
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	wfID := mcp.StrArg(args, "workflow_id")
	if wfID != "" {
		if _, err := d.store.Get(wfID); err != nil {
			return "", fmt.Errorf("workflow not found")
		}
	}
	runs, err := d.store.ListRuns(wfID, limit)
	if err != nil {
		return "", fmt.Errorf("failed to list executions: %w", err)
	}
	if len(runs) == 0 {
		return "No executions yet.", nil
	}

	names := map[string]string{}
	if wfs, werr := d.store.List(); werr == nil {
		for _, wf := range wfs {
			names[wf.ID] = wf.Name
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d execution(s), newest first:\n", len(runs))
	for _, run := range runs {
		e := run.Execution
		name := names[e.WorkflowID]
		if name == "" {
			name = e.WorkflowID
		}
		line := fmt.Sprintf("- %s | %s | %s | started %s", e.ID, name, e.Status, e.CreatedAt.Format("2006-01-02 15:04"))
		if e.StartedAt != nil && e.FinishedAt != nil {
			line += fmt.Sprintf(" | took %s", e.FinishedAt.Sub(*e.StartedAt).Round(time.Second))
		}
		if e.Error != "" {
			line += " | error=" + mcpTruncate(e.Error, 80)
		}
		fmt.Fprintf(&b, "%s\n", line)
	}
	return b.String(), nil
}

// mcpCancelExecution refuses, in plain words. The hosted server can cancel
// because a run lives in a queue it owns; here a run is a goroutine holding a
// context nothing else has a handle on. Marking the run "cancelled" in the
// store would stop the file from saying what the machine is actually doing —
// the goroutine would keep calling providers and keep spending the user's
// money — so the tool says what it cannot do instead.
func (d *daemon) mcpCancelExecution(execID string) (string, error) {
	if execID == "" {
		return "", fmt.Errorf("execution_id required")
	}
	run, err := d.store.GetRun(execID)
	if err != nil {
		return "", fmt.Errorf("execution not found")
	}
	if isTerminalStatus(run.Status) {
		return fmt.Sprintf("Execution %s already finished (%s); nothing to cancel.", execID, run.Status), nil
	}
	return fmt.Sprintf(
		"Execution %s is %s and cannot be cancelled: the local daemon runs workflows in-process and has no cancellation mechanism. "+
			"It will stop on its own when it finishes or after %s. Quitting Zyvro Studio stops it too.",
		execID, run.Status, executionTimeout,
	), nil
}

// waitForTerminal polls the run file until the execution reaches a terminal
// state, the wait elapses, or the client goes away. It returns the last state
// it saw.
func (d *daemon) waitForTerminal(ctx context.Context, execID string, wait time.Duration) (*localstore.Execution, error) {
	deadline := time.Now().Add(wait)
	for {
		run, err := d.store.GetRun(execID)
		if err != nil {
			return nil, err
		}
		if isTerminalStatus(run.Status) || !time.Now().Before(deadline) {
			return &run.Execution, nil
		}
		select {
		case <-ctx.Done():
			return &run.Execution, nil
		case <-time.After(mcpPollInterval):
		}
	}
}

// isTerminalStatus reports whether an execution has stopped for good.
func isTerminalStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

// describeNodeOutput summarizes a node output JSON for an LLM consumer:
// text snippets and absolute media URLs, never base64 payloads.
func describeNodeOutput(outJSON, origin string) string {
	var out struct {
		Type  string         `json:"type"`
		Value map[string]any `json:"value"`
	}
	if err := json.Unmarshal([]byte(outJSON), &out); err != nil {
		return ""
	}
	switch out.Type {
	case "image":
		if u, ok := out.Value["url"].(string); ok && u != "" {
			return "image=" + absoluteMediaURL(origin, u)
		}
		return "image (inline)"
	case "text":
		if t, ok := out.Value["text"].(string); ok {
			return "text=" + mcpTruncate(t, 200)
		}
	case "json":
		if b, err := json.Marshal(out.Value["data"]); err == nil {
			return "json=" + mcpTruncate(string(b), 200)
		}
	}
	return ""
}

// mcpOrigin is the origin this daemon was reached on. The hosted API reads its
// public origin from the environment because it is fixed; the daemon's is not —
// it binds a fresh random loopback port every launch — so the only honest
// source is the request that just arrived.
func mcpOrigin(r *http.Request) string {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + host
}

// absoluteMediaURL makes a stored media path absolute. Media URLs are stored
// host-relative so they survive a change of port, which also makes them
// useless to an MCP client: it is a separate process that has no page to
// resolve "/content/..." against. Data URLs and already-absolute URLs are
// returned untouched.
func absoluteMediaURL(origin, u string) string {
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "data:") {
		return u
	}
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return origin + u
}

// mediaFilename maps a /content/<file> URL back to the file in the project's
// media directory. The local media store is flat, so a legitimate URL has
// exactly one segment after /content/; anything with a separator in it did not
// come from SaveMedia and is refused rather than joined onto a path.
func mediaFilename(u string) (string, bool) {
	if u == "" || strings.HasPrefix(u, "data:") {
		return "", false
	}
	p := u
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		parsed, err := url.Parse(u)
		if err != nil {
			return "", false
		}
		p = parsed.Path
	}
	name, ok := strings.CutPrefix(p, "/content/")
	if !ok || name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", false
	}
	return name, true
}

// mimeForFile types an inlined image by its extension, which is all the local
// media store records: SaveMedia drops the mime type and keeps the extension.
func mimeForFile(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}

// mcpTruncate caps a summary line. It is the daemon's own, not the contract's:
// what a tool's prose looks like is this host's business, and the hosted server
// trims its chat transcripts with a copy of its own for the same reason.
func mcpTruncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
