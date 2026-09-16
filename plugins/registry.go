package plugins

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Registry is the set of node types the installed packs contribute: the dynamic
// half of what the engine's switch statement is the static half of.
//
// It deliberately knows nothing about engine. engine imports this package so it
// can fall through to a pack when its own switch has no case for a node type,
// and an import the other way would be a cycle. The few shapes that have to
// cross — Output, FileAccess — are mirrored here rather than shared.
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeDef
	packs map[string]bool
	// reserved are the node types the host implements itself and no pack may
	// take. They arrive from outside because this package cannot import the one
	// that knows them, and they are held here rather than in a literal so there
	// is only ever one such list in the product.
	reserved []string
	// builtin is the name of the pack the host bundles, if one was installed
	// through InstallBuiltin. It is the one pack allowed to define reserved
	// names, and the one pack whose types a later pack may not take.
	builtin string
}

// NewRegistry returns an empty registry that will refuse any pack defining one
// of the reserved node types.
func NewRegistry(reserved []string) *Registry {
	return &Registry{
		nodes:    map[string]*NodeDef{},
		packs:    map[string]bool{},
		reserved: append([]string(nil), reserved...),
	}
}

// Install adds every node of a pack, or none of them.
//
// All or nothing matters here: a pack half-installed because its fourth node
// collided would leave three node types registered from a pack the user was
// told had failed, and the workflow that then used one of them would work until
// the day it did not.
func (r *Registry) Install(p *Pack) error { return r.install(p, false) }

// InstallBuiltin installs the host's own bundled pack: the one pack that is
// allowed to define the reserved node types, because it is the pack they were
// reserved for. Everything installed after it is an ordinary pack and may not
// take any name this one took.
//
// It is a separate method rather than a flag on the pack, so that "this is the
// pack that ships inside the binary" is a decision the host makes at the call
// site and not something a manifest on disk can claim about itself.
func (r *Registry) InstallBuiltin(p *Pack) error { return r.install(p, true) }

func (r *Registry) install(p *Pack, builtin bool) error {
	if p == nil {
		return fmt.Errorf("no pack to install")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.packs[p.Manifest.Name] {
		return fmt.Errorf("pack %q is already installed", p.Manifest.Name)
	}
	for _, def := range p.Nodes {
		// Checked again here and not only at load: a built-in added to the
		// engine after a pack was written would otherwise be shadowed by a pack
		// that loaded cleanly last week.
		if !builtin && contains(r.reserved, def.Type) {
			return fmt.Errorf("pack %q defines %q, which is a built-in Zyvro node", p.Manifest.Name, def.Type)
		}
		if existing, ok := r.nodes[def.Type]; ok {
			// A collision with the bundled pack is not a collision between two
			// packs, it is an attempt to shadow a built-in, and saying so is
			// what tells the user whether they have two packs to choose between
			// or one pack to distrust.
			if existing.Pack == r.builtin && r.builtin != "" {
				return fmt.Errorf("pack %q defines %q, which is a built-in Zyvro node", p.Manifest.Name, def.Type)
			}
			return fmt.Errorf("pack %q defines %q, which pack %q already provides", p.Manifest.Name, def.Type, existing.Pack)
		}
	}
	for _, def := range p.Nodes {
		r.nodes[def.Type] = def
	}
	r.packs[p.Manifest.Name] = true
	if builtin {
		r.builtin = p.Manifest.Name
	}
	return nil
}

// Reserved returns the node types no pack may define, so a caller that loads a
// pack itself can pass the registry's own list to Load rather than keeping a
// second copy of it.
func (r *Registry) Reserved() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.reserved...)
}

// IsBuiltin reports whether a node type came from the bundled pack. The palette
// asks, because a node the engine ships is not a node to badge as a pack's, and
// the replay cache asks, because the bundled pack's Lua is compiled into the
// binary and changes only when the binary does.
func (r *Registry) IsBuiltin(nodeType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.nodes[nodeType]
	return ok && r.builtin != "" && def.Pack == r.builtin
}

// Kind returns a registered node type. The definition it returns is the live
// one, because Run needs the compiled chunk hanging off it; callers must treat
// it as read-only. Kinds is the copy-returning form for anything that only
// wants to display.
func (r *Registry) Kind(nodeType string) (*NodeDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.nodes[nodeType]
	return def, ok
}

// Kinds returns every registered node, ordered by type so the palette does not
// rearrange itself between launches.
func (r *Registry) Kinds() []NodeDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NodeDef, 0, len(r.nodes))
	for _, def := range r.nodes {
		out = append(out, def.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// Packs returns the names of the installed packs, for display.
func (r *Registry) Packs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.packs))
	for name := range r.packs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Run executes one pack node.
func (r *Registry) Run(ctx context.Context, nodeType string, in HostInput) (*Output, error) {
	out, _, err := r.RunWithLog(ctx, nodeType, in)
	return out, err
}

// RunWithLog is Run, also returning whatever the node printed or logged. The
// log comes back even when the run failed, because a node that timed out after
// logging three lines is a node whose author needs those three lines.
func (r *Registry) RunWithLog(ctx context.Context, nodeType string, in HostInput) (*Output, []string, error) {
	def, ok := r.Kind(nodeType)
	if !ok {
		return nil, nil, fmt.Errorf("unknown node type: %s", nodeType)
	}
	return runNode(ctx, def, in)
}
