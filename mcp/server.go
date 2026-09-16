package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ErrUnknownTool is what a ToolCaller returns for a name it does not implement.
// It is the one tool failure that is not a tool result: the client asked for
// something that is not on the advertised surface, which is a mistake in the
// call rather than a run that went wrong, so it comes back as a protocol error.
var ErrUnknownTool = errors.New("unknown tool")

// ToolCaller is the host's half of tools/call. By the time it is asked, the
// envelope and the arguments have been decoded and the name is one the client
// read out of the catalogue; everything after that — which store to read, whose
// data it is, what a run costs — is the host's business and none of this
// package's.
//
// An error is reported to the client as a tool result carrying isError, so the
// model can see what went wrong and try something else. Returning
// ErrUnknownTool instead produces a protocol error.
type ToolCaller interface {
	CallTool(ctx context.Context, name string, args map[string]any) ([]Content, error)
}

// Server answers MCP messages for one host. The fields are the parts a client
// is allowed to see differ: who the server says it is, how it introduces
// itself, and how large a message it will read. Everything else about the
// exchange is fixed here.
type Server struct {
	// Name and Version identify the server in the initialize reply.
	Name    string
	Version string

	// Instructions tell the model how to start. The hosted server and the
	// daemon describe different worlds, so the text is the host's to write.
	Instructions string

	// Catalogue is what tools/list returns, normally Tools(nil) or
	// Tools(overrides) for the few descriptions a host must reword.
	Catalogue []Tool

	// MaxBodyBytes bounds one JSON-RPC message. Image inputs arrive as data
	// URLs inside tools/call, so a host that accepts them needs a limit
	// generous enough to carry a photograph and no more.
	MaxBodyBytes int64

	// OnWriteError, when set, is told about a reply that could not be put on
	// the wire. There is nothing left to answer with at that point, so this is
	// for the host's log and nothing else.
	OnWriteError func(error)
}

// Serve answers one POSTed MCP message. The host owns everything in front of
// it: the method check, CORS, and authentication — by the time Serve runs the
// caller has already proved it is allowed to be here.
func (s *Server) Serve(w http.ResponseWriter, r *http.Request, caller ToolCaller) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.MaxBodyBytes))
	if err != nil {
		s.writeError(w, nil, CodeParseError, "could not read request body: "+err.Error())
		return
	}

	req, rpcErr := ParseRequest(body)
	if rpcErr != nil {
		s.writeError(w, req.ID, rpcErr.Code, rpcErr.Message)
		return
	}

	// Notifications carry no id and must get an empty 202, never a result.
	if req.IsNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		s.writeResult(w, req.ID, s.InitializeResult(req.Params))
	case "ping":
		s.writeResult(w, req.ID, map[string]any{})
	case "tools/list":
		s.writeResult(w, req.ID, map[string]any{"tools": s.Catalogue})
	case "tools/call":
		s.callTool(r.Context(), w, req, caller)
	default:
		s.writeError(w, req.ID, CodeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) callTool(ctx context.Context, w http.ResponseWriter, req Request, caller ToolCaller) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(w, req.ID, CodeInvalidParams, "invalid params: "+err.Error())
		return
	}
	if params.Name == "" {
		s.writeError(w, req.ID, CodeInvalidParams, "invalid params: tool name required")
		return
	}

	args := map[string]any{}
	if trimmed := strings.TrimSpace(string(params.Arguments)); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			s.writeError(w, req.ID, CodeInvalidParams, "invalid params: arguments must be a JSON object: "+err.Error())
			return
		}
	}

	content, err := caller.CallTool(ctx, params.Name, args)
	switch {
	case errors.Is(err, ErrUnknownTool):
		s.writeError(w, req.ID, CodeInvalidParams, "unknown tool: "+params.Name)
	case err != nil:
		// Tool errors are reported as MCP tool results (isError), not
		// protocol errors, so the client can see the message.
		s.writeResult(w, req.ID, map[string]any{
			"content": []Content{{Type: "text", Text: "error: " + err.Error()}},
			"isError": true,
		})
	default:
		s.writeResult(w, req.ID, map[string]any{"content": content})
	}
}

func (s *Server) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	s.reportWriteError(WriteResult(w, id, result))
}

func (s *Server) writeError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	s.reportWriteError(WriteError(w, id, code, message))
}

func (s *Server) reportWriteError(err error) {
	if err != nil && s.OnWriteError != nil {
		s.OnWriteError(err)
	}
}
