package engine

import (
	"embed"
	"fmt"
	"sync"

	"github.com/Zyvro/Zyvro-engine/plugins"
)

// The pack the engine ships inside itself.
//
// Every node that used to be a case in executeWithInput is now a Lua file in
// builtin/nodes. That is the whole point of the change: a node type somebody
// can read is a node type somebody can fork, and the node store the product is
// growing is worth very little if half the palette is invisible Go.
//
// What did not move is the privileged work. Generating an image, running the
// matte protocol, describing a picture to a model, running an agent loop: all
// of that is still the Go it always was, reached through a host function that
// only exists on a node whose pack declared the matching capability. So the Lua
// is the node — its ports, its settings, which host function its settings go
// to — and the Go is the work. Widening the sandbox instead would have handed
// those same powers to every pack installed from a store, which is the opposite
// of what the sandbox is for.
//
//go:embed builtin/zyvro-pack.json builtin/nodes/*.lua
var builtinPackFS embed.FS

// BuiltinPackName is the name in builtin/zyvro-pack.json. A node whose Pack is
// this one came from us, and both the palette and the replay cache ask.
const BuiltinPackName = "builtin"

// The pack is embedded rather than read from a directory, and that is a
// security property rather than a packaging convenience. A built-in read off
// disk is a built-in anything with write access to that disk can replace — and
// "llm" is a node type every workflow in the product uses. It also means the
// engine does not depend on a file layout the desktop app may not have: the
// nodes are in the binary, so there is no configuration under which they are
// missing.
//
// Parsed once. Compiling ten Lua chunks is quick but not free, and a registry
// is built every time a project reloads its packs. The definitions are
// immutable after loading — the compiled chunk is what is kept, never a live
// closure — so one parsed copy can safely back every registry in the process.
var builtinPack = sync.OnceValues(func() (*plugins.Pack, error) {
	// Loaded with no reserved names: this is the pack the names are reserved
	// for. It is the only pack loaded that way, and InstallBuiltin is the only
	// way it reaches a registry.
	return plugins.LoadFS(builtinPackFS, "builtin", nil)
})

// BuiltinPack returns the bundled pack, parsed once.
func BuiltinPack() (*plugins.Pack, error) { return builtinPack() }

// NewRegistry returns the registry a host should run graphs with: the bundled
// pack installed first, and the node types the engine still implements in Go
// reserved so no pack can take them.
//
// Installing the bundled pack first is what stops a store pack shadowing a
// built-in: the collision is refused at install, and the message says the name
// belongs to a built-in rather than to some other pack.
//
// It panics if the embedded pack does not load. There is no sensible engine
// without its own nodes, and a pack compiled into the binary either loads on
// every machine or on none: a failure here is a broken build, not a runtime
// condition a caller could handle. The test next door is what makes sure the
// build is not broken.
func NewRegistry() *plugins.Registry {
	reg := plugins.NewRegistry(ReservedNodeTypes())
	pack, err := BuiltinPack()
	if err != nil {
		panic(fmt.Sprintf("engine: the bundled node pack does not load, which means this binary was built wrong: %v", err))
	}
	if err := reg.InstallBuiltin(pack); err != nil {
		panic(fmt.Sprintf("engine: the bundled node pack does not install: %v", err))
	}
	return reg
}

// defaultRegistry is the registry a Runtime uses when its host did not give it
// one. A Runtime with no packs still has to be able to run "llm", and before
// this change it could, because "llm" was a case in a switch. Nothing about
// moving it into the bundled pack should make a caller that never heard of
// packs stop working.
var defaultRegistry = sync.OnceValue(NewRegistry)

// registry is the one this run should use: the host's, or the bundled one.
func (r *Runtime) registry() *plugins.Registry {
	if r.Plugins != nil {
		return r.Plugins
	}
	return defaultRegistry()
}
