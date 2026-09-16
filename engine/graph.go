package engine

import (
	"encoding/json"
	"fmt"
)

// Graph is the serialized React Flow graph stored in a workflow.
type Graph struct {
	Nodes    []GraphNode `json:"nodes"`
	Edges    []GraphEdge `json:"edges"`
	Viewport *struct {
		X    float64 `json:"x"`
		Y    float64 `json:"y"`
		Zoom float64 `json:"zoom"`
	} `json:"viewport,omitempty"`
}

type GraphNode struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Position struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"position"`
	Data     map[string]any `json:"data"`
	Selected bool           `json:"selected,omitempty"`
	Width    *float64       `json:"width,omitempty"`
	Height   *float64       `json:"height,omitempty"`
}

type GraphEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	// SourceHandle/TargetHandle carry the port identifier, e.g. "out", "in".
	SourceHandle string `json:"sourceHandle,omitempty"`
	TargetHandle string `json:"targetHandle,omitempty"`
	// "data" for ordinary DAG dependencies, "tool" for Brain capability links.
	Type string `json:"type,omitempty"`
}

func ParseGraph(raw string) (*Graph, error) {
	var g Graph
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		return nil, fmt.Errorf("invalid graph JSON: %w", err)
	}
	return &g, nil
}

// TopoOrder returns node IDs in dependency order using data edges only.
// Nodes whose only edges are tool edges (Brain capabilities) are excluded:
// they are invoked by the Brain at runtime, not by the DAG loop.
// Returns an error if the graph has a data cycle or references a missing node.
func TopoOrder(g *Graph) ([]string, error) {
	nodes := map[string]bool{}
	for _, n := range g.Nodes {
		nodes[n.ID] = true
	}
	// dataTouched: node participates in the data flow via at least one
	// data edge (as source or target). Pure tool nodes are skipped by the DAG.
	dataTouched := map[string]bool{}
	for _, e := range g.Edges {
		if e.Type == "tool" {
			continue
		}
		if !nodes[e.Source] || !nodes[e.Target] {
			return nil, fmt.Errorf("edge references missing node (%s -> %s)", e.Source, e.Target)
		}
		dataTouched[e.Source] = true
		dataTouched[e.Target] = true
	}

	dependents := map[string][]string{}
	degree := map[string]int{}
	for _, n := range g.Nodes {
		degree[n.ID] = 0
	}
	for _, e := range g.Edges {
		if e.Type == "tool" {
			continue
		}
		dependents[e.Source] = append(dependents[e.Source], e.Target)
		degree[e.Target]++
	}
	var queue []string
	for _, n := range g.Nodes {
		if degree[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	var order []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if !dataTouched[id] {
			// Pure tool node (isolated from the data flow): not executed here.
			continue
		}
		order = append(order, id)
		for _, dep := range dependents[id] {
			degree[dep]--
			if degree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	// Every node must be either DAG-ordered or an isolated tool node.
	included := map[string]bool{}
	for _, id := range order {
		included[id] = true
	}
	for _, n := range g.Nodes {
		if !included[n.ID] && dataTouched[n.ID] {
			return nil, fmt.Errorf("graph contains a cycle or unreachable node %s", n.ID)
		}
	}
	return order, nil
}

// UpstreamOf returns the data-edge sources feeding a node, ordered by edge id.
func UpstreamOf(g *Graph, nodeID string) []string {
	var out []string
	for _, e := range g.Edges {
		if e.Type == "tool" || e.Target != nodeID {
			continue
		}
		out = append(out, e.Source)
	}
	return out
}

// ToolNodesOf returns nodes connected to nodeID via tool edges (Brain links).
func ToolNodesOf(g *Graph, nodeID string) []string {
	var out []string
	for _, e := range g.Edges {
		if e.Type != "tool" {
			continue
		}
		if e.Target == nodeID {
			out = append(out, e.Source)
		}
	}
	return out
}

// NodeByID returns the graph node with the given ID.
func NodeByID(g *Graph, id string) *GraphNode {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

// SubgraphForNode returns the subgraph containing nodeID and all its
// data-edge ancestors, with the edges between them. Cached ancestors will
// be replayed by the runtime; uncached ones execute normally.
func SubgraphForNode(g *Graph, nodeID string) (*Graph, error) {
	if NodeByID(g, nodeID) == nil {
		return nil, fmt.Errorf("target node %s not found", nodeID)
	}
	// Walk ancestors transitively.
	included := map[string]bool{nodeID: true}
	queue := []string{nodeID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, src := range UpstreamOf(g, id) {
			if !included[src] {
				included[src] = true
				queue = append(queue, src)
			}
		}
	}
	sub := &Graph{}
	for _, n := range g.Nodes {
		if included[n.ID] {
			sub.Nodes = append(sub.Nodes, n)
		}
	}
	for _, e := range g.Edges {
		if included[e.Source] && included[e.Target] {
			sub.Edges = append(sub.Edges, e)
		}
	}
	return sub, nil
}
