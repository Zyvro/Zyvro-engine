// Package mcp is the half of Zyvro's MCP (Model Context Protocol) server that
// two hosts must implement identically: the hosted server, which is backed by
// MongoDB and a user account, and the local daemon, which is backed by the
// project folder on a laptop and has no accounts at all.
//
// The two are deliberately one surface. A client configured against the hosted
// server and then pointed at the daemon has to find the same seven zyvro_*
// tools, the same input schemas, the same JSON-RPC envelope and the same
// protocol negotiation, because nothing tells it that it changed servers. Kept
// as two copies of the same code, that surface stays identical only for as long
// as someone remembers to edit both — and the two copies are about to end up in
// different repositories, where the drift between them would no longer be
// visible to anyone.
//
// So this package holds the contract and nothing else: the envelope, the
// version negotiation, the tool catalogue as data, and a handler skeleton that
// routes a message and hands the actual tool work to the host through
// ToolCaller. What the tools then do — read a database or a directory, charge
// an account or not, cancel a run or explain that it cannot — is the host's,
// and stays there.
//
// It depends on the standard library only, so it can move into the public
// library with the daemon.
package mcp
