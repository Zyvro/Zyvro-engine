package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testTools answers every catalogued name and nothing else, so the tests below
// exercise the skeleton rather than a host's tools.
type testTools struct {
	lastName string
	lastArgs map[string]any
}

func (tt *testTools) CallTool(_ context.Context, name string, args map[string]any) ([]Content, error) {
	tt.lastName, tt.lastArgs = name, args
	switch name {
	case ToolListWorkflows:
		return TextResult("one workflow", nil)
	case ToolRunWorkflow:
		return nil, errors.New("workflow not found")
	}
	return nil, ErrUnknownTool
}

func newTestServer() (*Server, *testTools) {
	tools := &testTools{}
	return &Server{
		Name:         "zyvro",
		Version:      "0.0.0",
		Instructions: "start with zyvro_list_workflows",
		Catalogue:    Tools(nil),
		MaxBodyBytes: 1 << 20,
	}, tools
}

func post(s *Server, caller ToolCaller, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Serve(rec, r, caller)
	return rec
}

// envelope is one reply as a client reads it. Result and Error stay raw so a
// test can assert that exactly one of them was sent.
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *Error          `json:"error"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not a JSON-RPC envelope: %s", rec.Body.String())
	}
	if e.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q, want \"2.0\"", e.JSONRPC)
	}
	return e
}

// A notification carries no id and expects no answer. Replying to one puts a
// message on the wire the client is not waiting for, and every reply after it
// is then read against the wrong request. Both servers carried this warning as
// a comment over their own copy of the check; it is one check now, tested here.
func TestNotificationGetsNoResponse(t *testing.T) {
	s, tools := newTestServer()
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		// No id at all, on a method that would otherwise return a result.
		`{"jsonrpc":"2.0","method":"tools/list"}`,
		// An explicit null id, which JSON-RPC treats the same way.
		`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`,
		// Including a call that would otherwise have run a tool.
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"zyvro_list_workflows"}}`,
	} {
		rec := post(s, tools, body)
		if rec.Code != http.StatusAccepted {
			t.Errorf("%s: status = %d, want 202", body, rec.Code)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "" {
			t.Errorf("%s: body = %q, want nothing", body, got)
		}
	}
	if tools.lastName != "" {
		t.Errorf("a notification reached the tools as %q", tools.lastName)
	}
}

// A body we cannot parse is a JSON-RPC parse error carried in a 200. An HTTP
// 400 reaches the client as a dead transport with nothing in it to report, so
// it has nothing to show the user and nothing to retry.
func TestMalformedBodyIsAParseErrorOver200(t *testing.T) {
	s, tools := newTestServer()
	for _, body := range []string{"", "{", "not json at all", `{"jsonrpc":"2.0",`, `"a string"`} {
		rec := post(s, tools, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("body %q: status = %d, want 200", body, rec.Code)
		}
		e := decode(t, rec)
		if e.Error == nil || e.Error.Code != CodeParseError {
			t.Errorf("body %q: error = %+v, want code %d", body, e.Error, CodeParseError)
		}
		if len(e.Result) != 0 {
			t.Errorf("body %q: a failed request carried a result: %s", body, e.Result)
		}
		if strings.TrimSpace(string(e.ID)) != "null" {
			t.Errorf("body %q: id = %s, want null — there was no id to echo", body, e.ID)
		}
	}
}

// Batching was dropped from the protocol in 2025-06-18, and a batch silently
// treated as one message would answer the wrong call.
func TestBatchIsRefused(t *testing.T) {
	s, tools := newTestServer()
	e := decode(t, post(s, tools, `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`))
	if e.Error == nil || e.Error.Code != CodeInvalidRequest {
		t.Errorf("error = %+v, want code %d", e.Error, CodeInvalidRequest)
	}
}

// The id is echoed exactly as it arrived, whatever JSON type the client chose:
// that is how it matches the reply to the call it sent.
func TestIDIsEchoedVerbatim(t *testing.T) {
	s, tools := newTestServer()
	for _, id := range []string{`1`, `"abc"`, `0`, `-7`} {
		e := decode(t, post(s, tools, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"ping"}`, id)))
		if string(e.ID) != id {
			t.Errorf("id = %s, want %s", e.ID, id)
		}
	}
}

func TestInitializeNegotiatesProtocol(t *testing.T) {
	s, tools := newTestServer()
	for _, tc := range []struct{ asked, want string }{
		{"2025-06-18", "2025-06-18"},
		{"2025-03-26", "2025-03-26"},
		{"2024-11-05", "2024-11-05"},
		// A version we do not speak is answered with the newest one, which lets
		// the client decide whether to continue rather than refusing it.
		{"1999-01-01", ProtocolVersions[0]},
		{"", ProtocolVersions[0]},
	} {
		params := `{}`
		if tc.asked != "" {
			params = fmt.Sprintf(`{"protocolVersion":%q}`, tc.asked)
		}
		e := decode(t, post(s, tools, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":%s}`, params)))
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
		if err := json.Unmarshal(e.Result, &result); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if result.ProtocolVersion != tc.want {
			t.Errorf("asked %q: protocolVersion = %q, want %q", tc.asked, result.ProtocolVersion, tc.want)
		}
		if result.Capabilities.Tools == nil {
			t.Errorf("asked %q: the server did not advertise tools", tc.asked)
		}
		if result.ServerInfo.Name != s.Name || result.ServerInfo.Version != s.Version {
			t.Errorf("asked %q: serverInfo = %+v, want the host's", tc.asked, result.ServerInfo)
		}
		if result.Instructions != s.Instructions {
			t.Errorf("asked %q: instructions = %q, want the host's", tc.asked, result.Instructions)
		}
	}
}

// A tool that cannot do its job still answers with a result. A protocol error
// tells the client the server is broken; isError tells the model what went
// wrong, which is the thing it can act on.
func TestToolFailureIsAToolResult(t *testing.T) {
	s, tools := newTestServer()
	e := decode(t, post(s, tools, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zyvro_run_workflow","arguments":{}}}`))
	if e.Error != nil {
		t.Fatalf("transport error %+v, want a tool result", e.Error)
	}
	var result struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	if err := json.Unmarshal(e.Result, &result); err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Errorf("isError = false, want true")
	}
	if len(result.Content) != 1 || result.Content[0].Text != "error: workflow not found" {
		t.Errorf("content = %+v", result.Content)
	}
}

// An unknown tool name, by contrast, is a protocol error: the client sent
// something that is not part of the advertised surface.
func TestUnknownToolIsAProtocolError(t *testing.T) {
	s, tools := newTestServer()
	e := decode(t, post(s, tools, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zyvro_nope"}}`))
	if e.Error == nil || e.Error.Code != CodeInvalidParams {
		t.Fatalf("error = %+v, want code %d", e.Error, CodeInvalidParams)
	}
	if e.Error.Message != "unknown tool: zyvro_nope" {
		t.Errorf("message = %q", e.Error.Message)
	}
}

func TestUnknownMethodIsAProtocolError(t *testing.T) {
	s, tools := newTestServer()
	e := decode(t, post(s, tools, `{"jsonrpc":"2.0","id":1,"method":"no/such/method"}`))
	if e.Error == nil || e.Error.Code != CodeMethodNotFound {
		t.Errorf("error = %+v, want code %d", e.Error, CodeMethodNotFound)
	}
}

// Arguments are handed to the host as a decoded object. An absent or null
// arguments member is an empty object, not a failure: plenty of clients omit
// it for a tool that takes nothing.
func TestArgumentsDecoding(t *testing.T) {
	s, tools := newTestServer()
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zyvro_list_workflows"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zyvro_list_workflows","arguments":null}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zyvro_list_workflows","arguments":{}}}`,
	} {
		tools.lastArgs = nil
		if e := decode(t, post(s, tools, body)); e.Error != nil {
			t.Errorf("%s: %+v", body, e.Error)
		}
		if tools.lastArgs == nil || len(tools.lastArgs) != 0 {
			t.Errorf("%s: args = %v, want an empty object", body, tools.lastArgs)
		}
	}
	// Arguments that are not an object are a caller mistake, not a tool failure.
	e := decode(t, post(s, tools, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"zyvro_list_workflows","arguments":7}}`))
	if e.Error == nil || e.Error.Code != CodeInvalidParams {
		t.Errorf("error = %+v, want code %d", e.Error, CodeInvalidParams)
	}
}

// The catalogue is the contract with clients that were configured against the
// other host: the same seven names, and schemas a client can validate
// arguments against before it ever sends them.
func TestCatalogue(t *testing.T) {
	wantRequired := map[string][]string{
		ToolListWorkflows:   nil,
		ToolWorkflowGraph:   {"workflow_id"},
		ToolRunWorkflow:     {"workflow_id"},
		ToolExecutionStatus: {"execution_id"},
		ToolGetOutput:       {"execution_id"},
		ToolListExecutions:  nil,
		ToolCancelExecution: {"execution_id"},
	}
	tools := Tools(nil)
	if len(tools) != len(wantRequired) {
		t.Fatalf("tools = %d, want %d", len(tools), len(wantRequired))
	}
	seen := map[string]bool{}
	for _, tool := range tools {
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
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations, so a client cannot tell whether it is safe to auto-approve", tool.Name)
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
			list, ok := raw.([]string)
			if !ok {
				t.Fatalf("%s: required is not a string list: %v", tool.Name, raw)
			}
			for _, s := range list {
				// A required name that is not a declared property is a schema
				// no client could ever satisfy.
				if _, ok := props[s]; !ok {
					t.Errorf("%s requires %q, which it does not declare as a property", tool.Name, s)
				}
			}
			required = list
		}
		if strings.Join(required, ",") != strings.Join(req, ",") {
			t.Errorf("%s: required = %v, want %v", tool.Name, required, req)
		}
	}

	// wait_seconds promises exactly what WaitArg enforces.
	for _, tool := range tools {
		props, _ := tool.InputSchema["properties"].(map[string]any)
		ws, ok := props["wait_seconds"].(map[string]any)
		if !ok {
			continue
		}
		if ws["maximum"] != MaxWaitSeconds {
			t.Errorf("%s: wait_seconds maximum = %v, want %d", tool.Name, ws["maximum"], MaxWaitSeconds)
		}
	}
	if got := WaitArg(map[string]any{"wait_seconds": MaxWaitSeconds + 100}); got.Seconds() != MaxWaitSeconds {
		t.Errorf("WaitArg clamped to %s, not the %d seconds the schema advertises", got, MaxWaitSeconds)
	}
}

// A host may reword a description where the shared sentence would be a lie in
// its world, and nothing else. Names and schemas are what a client validates
// against, so an override must not be able to reach them.
func TestDescriptionOverrides(t *testing.T) {
	plain := Tools(nil)
	reworded := Tools(map[string]string{ToolCancelExecution: "cannot cancel here"})
	if len(plain) != len(reworded) {
		t.Fatalf("overriding a description changed the catalogue size")
	}
	for i := range plain {
		if plain[i].Name != reworded[i].Name {
			t.Errorf("tool %d: name changed", i)
		}
		if fmt.Sprint(plain[i].InputSchema) != fmt.Sprint(reworded[i].InputSchema) {
			t.Errorf("%s: input schema changed", plain[i].Name)
		}
		want := plain[i].Description
		if plain[i].Name == ToolCancelExecution {
			want = "cannot cancel here"
		}
		if reworded[i].Description != want {
			t.Errorf("%s: description = %q, want %q", plain[i].Name, reworded[i].Description, want)
		}
	}
	// An override naming no tool changes nothing rather than adding one.
	if len(Tools(map[string]string{"zyvro_nope": "x"})) != len(plain) {
		t.Errorf("an override for an unknown tool added something to the catalogue")
	}
}

// Each call gets its own maps, so a host that edits a schema it was handed
// cannot reach the next caller's copy.
func TestToolsAreNotShared(t *testing.T) {
	first := Tools(nil)
	first[0].InputSchema["type"] = "tampered"
	if got := Tools(nil)[0].InputSchema["type"]; got != "object" {
		t.Errorf("schema type = %v, want \"object\" — the catalogue is shared state", got)
	}
}

func TestIntArgReadsTheShapesClientsSend(t *testing.T) {
	for _, tc := range []struct {
		raw  any
		want int
	}{
		{float64(12), 12},
		{12, 12},
		{json.Number("12"), 12},
		{" 12 ", 12},
		{"not a number", 0},
		{nil, 0},
		{true, 0},
	} {
		if got := IntArg(map[string]any{"n": tc.raw}, "n"); got != tc.want {
			t.Errorf("IntArg(%#v) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// A body past the host's limit is still a JSON-RPC answer, not a dead socket.
func TestOversizedBodyIsAParseError(t *testing.T) {
	s, tools := newTestServer()
	s.MaxBodyBytes = 16
	e := decode(t, post(s, tools, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"padding":"aaaaaaaaaaaaaaaaaaaaaaaa"}}`))
	if e.Error == nil || e.Error.Code != CodeParseError {
		t.Errorf("error = %+v, want code %d", e.Error, CodeParseError)
	}
}
