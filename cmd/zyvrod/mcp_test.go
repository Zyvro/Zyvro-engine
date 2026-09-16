package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/localstore"
)

// mcpResponse is one JSON-RPC envelope as a client would read it. Result and
// Error are raw so a test can assert that exactly one of them was sent.
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// mcpToolResult is the tools/call result shape: content blocks plus the flag
// that says the tool itself failed, as opposed to the transport.
type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError"`
}

// text joins the text blocks, which is what a client would show the model.
func (r mcpToolResult) text() string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// mcpCall sends one authenticated JSON-RPC request with an id, which is what
// every MCP client call looks like.
func (e *testEnv) mcpCall(method string, params any) *httptest.ResponseRecorder {
	e.t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	return e.do(http.MethodPost, "/mcp", body)
}

// mcpDecode insists on a 200 carrying a well-formed JSON-RPC envelope: every
// answer this server gives, success or failure, is shaped that way.
func (e *testEnv) mcpDecode(rec *httptest.ResponseRecorder) mcpResponse {
	e.t.Helper()
	if rec.Code != http.StatusOK {
		e.t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp mcpResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		e.t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if resp.JSONRPC != "2.0" {
		e.t.Fatalf("jsonrpc = %q, want \"2.0\"", resp.JSONRPC)
	}
	return resp
}

// mcpTool calls one tool and returns its result, failing the test if the
// transport refused the call: a tool that cannot do its job must still answer
// with a result, so an error here is a real defect.
func (e *testEnv) mcpTool(name string, args map[string]any) mcpToolResult {
	e.t.Helper()
	resp := e.mcpDecode(e.mcpCall("tools/call", map[string]any{"name": name, "arguments": args}))
	if resp.Error != nil {
		e.t.Fatalf("%s: transport error %d: %s", name, resp.Error.Code, resp.Error.Message)
	}
	var out mcpToolResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		e.t.Fatalf("%s: decode result %s: %v", name, resp.Result, err)
	}
	return out
}

// mcpRawBody posts a body byte for byte, without a JSON encoder in the way, so
// a test can send something that is not JSON at all.
func (e *testEnv) mcpRawBody(body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.daemon.token)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

var execIDPattern = regexp.MustCompile(`execution_id=([a-f0-9]{24})`)

// The handshake between an MCP client and this server: it names a protocol
// version it speaks, and identifies itself well enough for the client to show
// the user what it just connected to.
func TestMCPInitialize(t *testing.T) {
	e := newTestEnv(t)

	for _, asked := range []string{"2025-06-18", "2024-11-05", "1999-01-01", ""} {
		params := map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}
		if asked != "" {
			params["protocolVersion"] = asked
		}
		resp := e.mcpDecode(e.mcpCall("initialize", params))
		if resp.Error != nil {
			t.Fatalf("initialize %q: %+v", asked, resp.Error)
		}
		var result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools *struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatalf("decode result: %v", err)
		}

		// A version we speak is echoed; anything else is answered with the
		// newest one so the client can decide whether to continue.
		want := mcpProtocolVersions[0]
		for _, v := range mcpProtocolVersions {
			if asked == v {
				want = v
			}
		}
		if result.ProtocolVersion != want {
			t.Errorf("asked %q: protocolVersion = %q, want %q", asked, result.ProtocolVersion, want)
		}
		if result.Capabilities.Tools == nil {
			t.Errorf("asked %q: the server did not advertise tools", asked)
		}
		if result.ServerInfo.Name == "" || result.ServerInfo.Version == "" {
			t.Errorf("asked %q: serverInfo = %+v", asked, result.ServerInfo)
		}
		if !strings.Contains(result.Instructions, "zyvro_list_workflows") {
			t.Errorf("asked %q: instructions do not tell the client where to start: %q", asked, result.Instructions)
		}
	}
}

// The tool surface is a contract with clients that were configured against the
// hosted server: the same seven names, and schemas a client can validate
// arguments against before it ever sends them.
func TestMCPToolsList(t *testing.T) {
	e := newTestEnv(t)
	resp := e.mcpDecode(e.mcpCall("tools/list", nil))
	if resp.Error != nil {
		t.Fatalf("tools/list: %+v", resp.Error)
	}
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}

	// Name -> the arguments a client must supply. Empty means the tool takes
	// none, which is still an object schema, not an absent one.
	wantRequired := map[string][]string{
		"zyvro_list_workflows":   {},
		"zyvro_workflow_graph":   {"workflow_id"},
		"zyvro_run_workflow":     {"workflow_id"},
		"zyvro_execution_status": {"execution_id"},
		"zyvro_get_output":       {"execution_id"},
		"zyvro_list_executions":  {},
		"zyvro_cancel_execution": {"execution_id"},
	}
	if len(result.Tools) != len(wantRequired) {
		t.Fatalf("tools = %d, want %d", len(result.Tools), len(wantRequired))
	}

	seen := map[string]bool{}
	for _, tool := range result.Tools {
		req, known := wantRequired[tool.Name]
		if !known {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		if seen[tool.Name] {
			t.Errorf("tool %q listed twice", tool.Name)
		}
		seen[tool.Name] = true

		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("%s has no description for the model to read", tool.Name)
		}
		if got, _ := tool.InputSchema["type"].(string); got != "object" {
			t.Errorf("%s: schema type = %v, want \"object\"", tool.Name, tool.InputSchema["type"])
		}
		props, ok := tool.InputSchema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: schema has no properties object: %+v", tool.Name, tool.InputSchema)
		}
		for name, raw := range props {
			prop, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("%s.%s is not a schema object", tool.Name, name)
				continue
			}
			switch prop["type"] {
			case "string", "integer", "number", "boolean", "object", "array":
			default:
				t.Errorf("%s.%s: type = %v, not a JSON Schema type", tool.Name, name, prop["type"])
			}
			if d, _ := prop["description"].(string); strings.TrimSpace(d) == "" {
				t.Errorf("%s.%s has no description", tool.Name, name)
			}
		}

		var required []string
		if raw, ok := tool.InputSchema["required"]; ok {
			list, ok := raw.([]any)
			if !ok {
				t.Fatalf("%s: required is not an array: %v", tool.Name, raw)
			}
			for _, v := range list {
				s, ok := v.(string)
				if !ok {
					t.Fatalf("%s: required entry %v is not a string", tool.Name, v)
				}
				// A required name that is not a declared property is a schema
				// no client could ever satisfy.
				if _, ok := props[s]; !ok {
					t.Errorf("%s requires %q, which it does not declare as a property", tool.Name, s)
				}
				required = append(required, s)
			}
		}
		if strings.Join(required, ",") != strings.Join(req, ",") {
			t.Errorf("%s: required = %v, want %v", tool.Name, required, req)
		}
	}
	for name := range wantRequired {
		if !seen[name] {
			t.Errorf("tools/list is missing %q", name)
		}
	}
}

// /mcp carries the same authority as /api — it can read the project and spend
// the user's provider credits — so it sits behind the same token. Anything else
// on the machine that guessed the port gets nothing.
func TestMCPRequiresToken(t *testing.T) {
	e := newTestEnv(t)
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}

	if rec := e.raw(http.MethodPost, "/mcp", body, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no header: status = %d, want 401", rec.Code)
	}
	wrong := map[string]string{"Authorization": "Bearer " + localstore.NewID()}
	if rec := e.raw(http.MethodPost, "/mcp", body, wrong); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
	raw := map[string]string{"Authorization": e.daemon.token}
	if rec := e.raw(http.MethodPost, "/mcp", body, raw); rec.Code != http.StatusUnauthorized {
		t.Errorf("token without the Bearer scheme: status = %d, want 401", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/mcp", body); rec.Code != http.StatusOK {
		t.Errorf("with the token: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// This server speaks JSON mode only, so the SSE half of the Streamable HTTP
// transport is refused rather than half-implemented.
func TestMCPRejectsNonPost(t *testing.T) {
	e := newTestEnv(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if rec := e.do(method, "/mcp", nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp: status = %d, want 405", method, rec.Code)
		}
	}
}

// The whole point of the endpoint: an agent that has only ever seen the tool
// list can find a workflow, read its graph, run it and see how it ended.
func TestMCPWorkflowRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	wf, err := e.store.Create("MCP Round Trip", json.RawMessage(providerFreeGraph))
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	list := e.mcpTool("zyvro_list_workflows", nil)
	if list.IsError {
		t.Fatalf("list: %s", list.text())
	}
	if !strings.Contains(list.text(), wf.ID) || !strings.Contains(list.text(), "MCP Round Trip") {
		t.Fatalf("the listing did not find the workflow:\n%s", list.text())
	}

	graph := e.mcpTool("zyvro_workflow_graph", map[string]any{"workflow_id": wf.ID})
	if graph.IsError {
		t.Fatalf("graph: %s", graph.text())
	}
	for _, want := range []string{"n1", "textInput", "n2", "output", "n1 -> n2"} {
		if !strings.Contains(graph.text(), want) {
			t.Errorf("the graph outline is missing %q:\n%s", want, graph.text())
		}
	}

	run := e.mcpTool("zyvro_run_workflow", map[string]any{"workflow_id": wf.ID})
	if run.IsError {
		t.Fatalf("run: %s", run.text())
	}
	m := execIDPattern.FindStringSubmatch(run.text())
	if m == nil {
		t.Fatalf("the run did not report an execution id:\n%s", run.text())
	}
	execID := m[1]
	// There is no queue locally, so the answer must not claim a position in one.
	if strings.Contains(strings.ToLower(run.text()), "queue") {
		t.Errorf("the run result mentions a queue that does not exist here:\n%s", run.text())
	}

	// wait_seconds is what makes this usable from a chat panel: one call, one
	// finished result, instead of a polling loop the model has to drive.
	status := e.mcpTool("zyvro_execution_status", map[string]any{
		"execution_id": execID,
		"wait_seconds": 15,
	})
	if status.IsError {
		t.Fatalf("status: %s", status.text())
	}
	if !strings.Contains(status.text(), "Execution "+execID+": completed") {
		t.Fatalf("the run did not reach a terminal state:\n%s", status.text())
	}
	if !strings.Contains(status.text(), "hello from the daemon") {
		t.Errorf("the status does not carry the node output:\n%s", status.text())
	}

	// The same run is visible in the history, and scoped to its workflow.
	execs := e.mcpTool("zyvro_list_executions", map[string]any{"workflow_id": wf.ID})
	if execs.IsError || !strings.Contains(execs.text(), execID) {
		t.Errorf("the execution list did not find %s:\n%s", execID, execs.text())
	}

	// A text-only workflow has no images, which is an answer, not a failure.
	out := e.mcpTool("zyvro_get_output", map[string]any{"execution_id": execID})
	if out.IsError {
		t.Fatalf("get_output: %s", out.text())
	}
	if !strings.Contains(out.text(), "no image output") {
		t.Errorf("get_output = %q", out.text())
	}
}

// Media is stored host-relative so it survives the daemon picking a new port
// every launch. An MCP client is a separate process with no page to resolve
// that against, so what it is handed has to be absolute.
func TestMCPGetOutputReturnsAbsoluteMediaURLs(t *testing.T) {
	e := newTestEnv(t)
	execID := localstore.NewID()
	url, err := e.store.Media().SaveMedia(execID, "n1", "out.png", []byte("png-bytes"), "image/png")
	if err != nil {
		t.Fatalf("SaveMedia: %v", err)
	}
	if !strings.HasPrefix(url, "/content/") {
		t.Fatalf("SaveMedia returned %q, which this test's premise depends on", url)
	}

	// A finished run whose node produced that image, written the way the
	// recorder writes one.
	out, _ := json.Marshal(map[string]any{"type": "image", "value": map[string]any{"url": url}})
	if err := e.store.SaveRun(&localstore.Run{
		Execution: localstore.Execution{
			ID:         execID,
			WorkflowID: localstore.NewID(),
			UserID:     localstore.LocalUserID,
			Status:     "completed",
		},
		Nodes: []localstore.NodeExecution{{
			ID:          localstore.NewID(),
			ExecutionID: execID,
			NodeID:      "n1",
			NodeType:    "imageGen",
			Status:      "completed",
			OutputJSON:  string(out),
		}},
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	// httptest.NewRequest sends Host: example.com, which stands in for whatever
	// loopback port the daemon happens to have bound.
	got := e.mcpTool("zyvro_get_output", map[string]any{"execution_id": execID})
	if got.IsError {
		t.Fatalf("get_output: %s", got.text())
	}
	if !strings.Contains(got.text(), "http://example.com"+url) {
		t.Errorf("the media URL was not made absolute:\n%s", got.text())
	}
	// The image itself comes back inline, which is the reason the tool exists.
	var inlined int
	for _, c := range got.Content {
		if c.Type == "image" {
			inlined++
			if c.MimeType != "image/png" {
				t.Errorf("mimeType = %q, want image/png", c.MimeType)
			}
		}
	}
	if inlined != 1 {
		t.Errorf("inlined %d images, want 1", inlined)
	}

	// The status text points at the images by absolute URL too.
	status := e.mcpTool("zyvro_execution_status", map[string]any{"execution_id": execID})
	if !strings.Contains(status.text(), "image=http://example.com"+url) {
		t.Errorf("the status did not report an absolute media URL:\n%s", status.text())
	}
}

// A notification carries no id and expects no answer. Replying to one puts a
// message on the wire the client is not waiting for, and every reply after it
// is read against the wrong request.
func TestMCPNotificationGetsNoResponse(t *testing.T) {
	e := newTestEnv(t)
	for _, body := range []map[string]any{
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
		{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 1}},
		// No id at all, on a method that would otherwise return a result.
		{"jsonrpc": "2.0", "method": "tools/list"},
	} {
		rec := e.do(http.MethodPost, "/mcp", body)
		if rec.Code != http.StatusAccepted {
			t.Errorf("%v: status = %d, want 202", body["method"], rec.Code)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "" {
			t.Errorf("%v: body = %q, want nothing", body["method"], got)
		}
	}
}

// A body we cannot parse is a JSON-RPC parse error carried in a 200. An HTTP
// 400 reaches the client as a dead transport with nothing in it to report.
func TestMCPMalformedBodyIsAParseError(t *testing.T) {
	e := newTestEnv(t)
	for _, body := range []string{"", "{", "not json at all", `{"jsonrpc":"2.0",`} {
		rec := e.mcpRawBody(body)
		if rec.Code != http.StatusOK {
			t.Fatalf("body %q: status = %d, want 200", body, rec.Code)
		}
		var resp mcpResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body %q: response is not JSON-RPC: %s", body, rec.Body.String())
		}
		if resp.Error == nil || resp.Error.Code != -32700 {
			t.Errorf("body %q: error = %+v, want code -32700", body, resp.Error)
		}
		if len(resp.Result) != 0 {
			t.Errorf("body %q: a failed request carried a result: %s", body, resp.Result)
		}
	}
}

// Batching was dropped from the protocol in 2025-06-18, and a batch that were
// silently treated as one message would answer the wrong call.
func TestMCPRejectsBatches(t *testing.T) {
	e := newTestEnv(t)
	rec := e.mcpRawBody(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := e.mcpDecode(rec)
	if resp.Error == nil || resp.Error.Code != -32600 {
		t.Errorf("error = %+v, want code -32600", resp.Error)
	}
}

// A tool that cannot do its job still answers with a result. A protocol error
// tells the client the server is broken; isError tells the model what went
// wrong, which is the thing it can act on.
func TestMCPToolFailuresAreToolErrors(t *testing.T) {
	e := newTestEnv(t)
	missing := localstore.NewID()
	cases := []struct {
		name string
		args map[string]any
	}{
		{"zyvro_workflow_graph", map[string]any{"workflow_id": missing}},
		{"zyvro_run_workflow", map[string]any{"workflow_id": missing}},
		{"zyvro_list_executions", map[string]any{"workflow_id": missing}},
		{"zyvro_execution_status", map[string]any{"execution_id": missing}},
		{"zyvro_get_output", map[string]any{"execution_id": missing}},
		{"zyvro_cancel_execution", map[string]any{"execution_id": missing}},
		// A malformed id must read the same as a missing one, so the tool
		// cannot be used to probe the filesystem.
		{"zyvro_workflow_graph", map[string]any{"workflow_id": "../../etc/passwd"}},
		// A required argument that was not sent.
		{"zyvro_workflow_graph", map[string]any{}},
	}
	for _, c := range cases {
		got := e.mcpTool(c.name, c.args)
		if !got.IsError {
			t.Errorf("%s %v: isError = false, want a tool error (got %q)", c.name, c.args, got.text())
		}
		if strings.TrimSpace(got.text()) == "" {
			t.Errorf("%s %v: the tool error carries no message", c.name, c.args)
		}
	}

	// An unknown tool name, by contrast, is a protocol error: the client sent
	// something that is not part of the advertised surface.
	resp := e.mcpDecode(e.mcpCall("tools/call", map[string]any{"name": "zyvro_nope"}))
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("unknown tool: error = %+v, want code -32602", resp.Error)
	}
	resp = e.mcpDecode(e.mcpCall("no/such/method", nil))
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("unknown method: error = %+v, want code -32601", resp.Error)
	}
}

// The daemon cannot stop a run, so the tool says so. Reporting success and
// leaving the goroutine calling providers would be worse than refusing: the
// user would believe they had stopped spending money.
func TestMCPCancelRefusesRatherThanPretending(t *testing.T) {
	e := newTestEnv(t)
	execID := localstore.NewID()
	if err := e.store.SaveRun(&localstore.Run{
		Execution: localstore.Execution{ID: execID, UserID: localstore.LocalUserID, Status: "running"},
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	got := e.mcpTool("zyvro_cancel_execution", map[string]any{"execution_id": execID})
	if got.IsError {
		t.Fatalf("cancel: %s", got.text())
	}
	if !strings.Contains(got.text(), "cannot be cancelled") {
		t.Errorf("cancel did not refuse plainly:\n%s", got.text())
	}
	// And it did not quietly rewrite the run to look cancelled.
	after, err := e.store.GetRun(execID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if after.Status != "running" {
		t.Errorf("status = %q, want it left alone as \"running\"", after.Status)
	}

	// A run that is already over is the one case with a real answer.
	done := localstore.NewID()
	if err := e.store.SaveRun(&localstore.Run{
		Execution: localstore.Execution{ID: done, UserID: localstore.LocalUserID, Status: "completed"},
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if got := e.mcpTool("zyvro_cancel_execution", map[string]any{"execution_id": done}); !strings.Contains(got.text(), "already finished") {
		t.Errorf("finished run: %q", got.text())
	}
}

// ping keeps a client's connection check from looking like an unknown method.
func TestMCPPing(t *testing.T) {
	e := newTestEnv(t)
	resp := e.mcpDecode(e.mcpCall("ping", nil))
	if resp.Error != nil {
		t.Fatalf("ping: %+v", resp.Error)
	}
	if strings.TrimSpace(string(resp.Result)) != "{}" {
		t.Errorf("ping result = %s, want {}", resp.Result)
	}
}
