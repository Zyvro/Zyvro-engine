package mcp

import "encoding/json"

// ProtocolVersions are the versions this server implements, newest first.
// Treat it as read-only: both hosts hand the same slice out to their own
// package-level name, so writing through one of them would change the other.
var ProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Negotiate answers with the client's protocol version when this server speaks
// it, and with the newest supported version otherwise, which lets the client
// decide whether to continue rather than being refused outright.
func Negotiate(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	for _, v := range ProtocolVersions {
		if p.ProtocolVersion == v {
			return v
		}
	}
	return ProtocolVersions[0]
}

// InitializeResult is the reply to initialize: what version we settled on, what
// this server can do, who it says it is, and how to start using it. Name,
// version and instructions come from the host, because those are the parts a
// client is supposed to see differ between the hosted server and a daemon
// running on someone's laptop.
func (s *Server) InitializeResult(params json.RawMessage) map[string]any {
	return map[string]any{
		"protocolVersion": Negotiate(params),
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		"instructions":    s.Instructions,
	}
}
