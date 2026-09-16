package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Node fingerprinting: a node's fingerprint identifies "what would this
// node produce" — its type, its full config, the runtime inputs it may
// consume, and the fingerprints of everything upstream of it. When a new
// run computes the same fingerprint for a node that already completed in a
// previous execution, the cached output is replayed instead of calling any
// model again.

// NodeFingerprint is a stable hash of a node's execution inputs.
type NodeFingerprint string

// CacheEntry is one completed node execution available for replay.
type CacheEntry struct {
	NodeID string
	// Fingerprint the entry was recorded under.
	Fingerprint NodeFingerprint
	Output      *NodeOutput
	// SourceExecution is the execution the output came from (for the UI).
	SourceExecution string
}

// fingerprintValue hashes any JSON-serializable value deterministically.
// Maps are sorted by key so map iteration order never changes the result.
func fingerprintValue(h interface{ Write([]byte) (int, error) }, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = h.Write(b)
}

// NodeCode answers, for a node type implemented in Lua, a digest of the exact
// code that would run. A nil NodeCode, or a false second return, means "no
// script stands behind this type here" — which is the right answer for a Go
// switch case and for a host that loaded no packs.
//
// It is a parameter rather than something this file looks up because the
// answer belongs to a particular host: the same graph, on a machine where the
// user has edited a node's script, must fingerprint differently than it did
// before the edit, and on a machine with no packs at all the question does not
// arise. Runtime.ComputeFingerprints supplies the registry's answer.
type NodeCode func(nodeType string) (string, bool)

// FingerprintOf computes the fingerprint of a node in the current graph
// state. upstream are the fingerprints of the node's data-edge parents,
// aligned with UpstreamOf(g, nodeID).
func FingerprintOf(g *Graph, nodeID string, upstream []NodeFingerprint, runtimeInputs map[string]any, code NodeCode) (NodeFingerprint, error) {
	n := NodeByID(g, nodeID)
	if n == nil {
		return "", fmt.Errorf("node %s not found", nodeID)
	}
	h := sha256.New()
	h.Write([]byte("zyvro-node-v1\n"))
	h.Write([]byte(n.Type + "\n"))

	// The node's own code, when it has any. A type name is only a stable
	// description of behaviour while one implementation stands behind it, and
	// a pack node's implementation is a file the user can edit: without this,
	// editing a node and running again replayed the old answer forever.
	//
	// Absent when there is no script, which leaves the hash byte-for-byte what
	// it was before this existed — so every cache entry recorded for a Go node
	// stays valid.
	if code != nil {
		if digest, ok := code(n.Type); ok {
			h.Write([]byte("code:" + digest + "\n"))
		}
	}

	// Config: sorted-key canonical JSON so any map ordering is stable.
	cfg := nodeConfig(n)
	if keys := sortedKeys(cfg); len(keys) > 0 {
		h.Write([]byte("config:"))
		for _, k := range keys {
			kb, _ := json.Marshal(cfg[k])
			h.Write([]byte(k))
			h.Write([]byte("="))
			h.Write(kb)
			h.Write([]byte(";"))
		}
		h.Write([]byte("\n"))
	}

	// Runtime inputs the node itself consumes (input nodes read their
	// value from the run inputs by key or node id/label).
	if key := InputKey(n); key != "" {
		h.Write([]byte("input:" + key + "\n"))
		if v, ok := runtimeInputs[key]; ok {
			fingerprintValue(h, v)
			h.Write([]byte("\n"))
		}
	}

	// Upstream fingerprints, in a stable order.
	fps := append([]NodeFingerprint(nil), upstream...)
	sort.Slice(fps, func(i, j int) bool { return fps[i] < fps[j] })
	h.Write([]byte("upstream:"))
	for _, f := range fps {
		h.Write([]byte(string(f)))
		h.Write([]byte(";"))
	}
	h.Write([]byte("\n"))

	return NodeFingerprint(hex.EncodeToString(h.Sum(nil))), nil
}

// sortedKeys returns the sorted keys of a map[string]any.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ComputeFingerprints walks the graph in topological order and computes a
// fingerprint per node, including upstream fingerprints transitively.
// Returns a map nodeID -> fingerprint, or an error if the graph is invalid.
func ComputeFingerprints(g *Graph, runtimeInputs map[string]any, code NodeCode) (map[string]NodeFingerprint, map[string][]string, error) {
	order, err := TopoOrder(g)
	if err != nil {
		return nil, nil, err
	}
	fps := map[string]NodeFingerprint{}
	parents := map[string][]string{}
	for _, id := range order {
		ups := UpstreamOf(g, id)
		parents[id] = ups
		upFps := make([]NodeFingerprint, len(ups))
		for i, u := range ups {
			upFps[i] = fps[u]
		}
		fp, err := FingerprintOf(g, id, upFps, runtimeInputs, code)
		if err != nil {
			return nil, nil, err
		}
		fps[id] = fp
	}
	return fps, parents, nil
}

// LoadCache converts the node executions of a previous run into cache
// entries, keyed by node id. "cached" entries (replayed in that run) are as
// valid as "completed" ones: they carry the same fingerprinted output.
func LoadCache(nodes []CachedNodeResult) map[string]CacheEntry {
	entries := map[string]CacheEntry{}
	for _, n := range nodes {
		if (n.Status != "completed" && n.Status != "cached") || n.Fingerprint == "" || n.OutputJSON == "" {
			continue
		}
		var out NodeOutput
		if err := json.Unmarshal([]byte(n.OutputJSON), &out); err != nil {
			continue
		}
		entries[n.NodeID] = CacheEntry{
			NodeID:          n.NodeID,
			Fingerprint:     NodeFingerprint(n.Fingerprint),
			Output:          &out,
			SourceExecution: n.ExecutionID,
		}
	}
	return entries
}

// CachedNodeResult is the persisted form of a node execution as seen by
// the cache loader (decoupled from the mongo models).
type CachedNodeResult struct {
	NodeID      string
	ExecutionID string
	Status      string
	Fingerprint string
	OutputJSON  string
}

// String makes fingerprints printable.
func (f NodeFingerprint) String() string { return string(f) }

// nodeCode is this runtime's answer to NodeCode: the digest the registry holds
// for a Lua node, nothing for a type the Go switch answers itself.
func (r *Runtime) nodeCode(nodeType string) (string, bool) {
	reg := r.registry()
	if reg == nil {
		return "", false
	}
	def, ok := reg.Kind(nodeType)
	if !ok || def.CodeDigest == "" {
		return "", false
	}
	return def.CodeDigest, true
}

// ComputeFingerprints is the form to use when packs are in play: it hashes
// each Lua node's code into its fingerprint, so a node whose script changed no
// longer replays the answer the old script gave.
//
// The package-level function is still the right call for a host that has no
// packs — it takes the lookup as a parameter and nil says so.
func (r *Runtime) ComputeFingerprints(runtimeInputs map[string]any) (map[string]NodeFingerprint, map[string][]string, error) {
	return ComputeFingerprints(r.Graph, runtimeInputs, r.nodeCode)
}
