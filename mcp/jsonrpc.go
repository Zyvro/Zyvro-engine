package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
)

// The JSON-RPC 2.0 error codes this server uses. They are part of what a client
// reads to decide whether it sent something wrong or the server is broken, so
// they are named here rather than written as bare numbers at each call site.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
)

// Request is one JSON-RPC message as it arrives. ID stays raw: JSON-RPC lets a
// client use a number, a string or null, and the reply has to echo back exactly
// what was sent rather than a value we re-encoded from a guess at its type.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the message expects no response: JSON-RPC says
// that is any message without an id, and MCP names them "notifications/*".
// Answering one puts a response on the wire the client is not waiting for,
// after which every reply it reads is matched against the wrong request.
func (r Request) IsNotification() bool {
	if strings.HasPrefix(r.Method, "notifications/") {
		return true
	}
	id := strings.TrimSpace(string(r.ID))
	return id == "" || id == "null"
}

// Error is the error object of a JSON-RPC reply.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ParseRequest decodes one JSON-RPC message. A non-nil *Error is the reply to
// send back, and every one of them travels inside an HTTP 200: the client is
// speaking JSON-RPC and only reads the envelope, so a transport-level refusal
// reaches it as "the server broke" with nothing in it to act on.
func ParseRequest(body []byte) (Request, *Error) {
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		// JSON-RPC batching was dropped in MCP 2025-06-18 and never used here.
		return Request{}, &Error{
			Code:    CodeInvalidRequest,
			Message: "batched requests are not supported; send one JSON-RPC request per call",
		}
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return Request{}, &Error{Code: CodeParseError, Message: "parse error: " + err.Error()}
	}
	if req.JSONRPC != "2.0" && req.JSONRPC != "" {
		return req, &Error{Code: CodeInvalidRequest, Message: "invalid jsonrpc version"}
	}
	return req, nil
}

// resultReply and errorReply are separate types because a JSON-RPC reply
// carries a result or an error and never both. Their fields are declared in the
// order the wire already carried them: both servers built these replies from
// map[string]any, and encoding/json sorts map keys, so naming the fields
// changes no bytes.
type resultReply struct {
	ID      json.RawMessage `json:"id"`
	JSONRPC string          `json:"jsonrpc"`
	Result  any             `json:"result"`
}

type errorReply struct {
	Error   Error           `json:"error"`
	ID      json.RawMessage `json:"id"`
	JSONRPC string          `json:"jsonrpc"`
}

// WriteResult sends a successful reply for the request that carried id.
func WriteResult(w http.ResponseWriter, id json.RawMessage, result any) error {
	return writeEnvelope(w, resultReply{ID: id, JSONRPC: "2.0", Result: result})
}

// WriteError sends a JSON-RPC error for the request that carried id, or for no
// request at all when the body was unreadable and id is nil.
func WriteError(w http.ResponseWriter, id json.RawMessage, code int, message string) error {
	return writeEnvelope(w, errorReply{Error: Error{Code: code, Message: message}, ID: id, JSONRPC: "2.0"})
}

// writeEnvelope always answers 200. Success and failure alike are carried in
// the JSON-RPC envelope, which is the only thing the client parses.
func writeEnvelope(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(v)
}
