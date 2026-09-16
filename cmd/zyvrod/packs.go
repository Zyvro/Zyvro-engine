package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Zyvro/Zyvro-engine/engine"
	"github.com/Zyvro/Zyvro-engine/plugins"
)

// Packs are node types the project itself carries: a directory of Lua under
// .zyvro/packs, loaded when the project opens and then runnable exactly like a
// node the engine implements in Go.
//
// The rule that shapes this whole file is that a pack must never be able to
// stop a project from opening. Someone who pulls a branch with a half-finished
// pack on it, or edits a node file and gets the syntax wrong, has to end up
// looking at their workflows and an error message — not at an app that refuses
// to start and does not say why.

// packInfo is one installed pack as the settings panel sees it. A pack that
// failed to load is still an entry, carrying its reason: a pack that vanishes
// from the list when it breaks is a pack the user cannot debug.
type packInfo struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Author       string   `json:"author,omitempty"`
	Capabilities []string `json:"capabilities"`
	NodeTypes    []string `json:"node_types"`
	// Dir is where it was loaded from, so "why is this pack not what I edited"
	// has an answer that does not require guessing at the layout.
	Dir string `json:"dir"`
	// Error is empty for a pack that loaded. Anything else is the reason it did
	// not, verbatim, because the loader's messages already name the file and
	// the line and rewording them here would only lose that.
	Error string `json:"error,omitempty"`
}

// loadPacks reads .zyvro/packs and replaces the daemon's registry with what it
// found. It is called when the project opens and again on demand, and it builds
// a fresh registry every time rather than adding to the live one: a reload that
// merged would leave a deleted pack installed and a renamed node registered
// twice under the old name.
func (d *daemon) loadPacks() []packInfo {
	reg, infos := loadPacksFrom(d.store.PacksDir())
	d.mu.Lock()
	d.plugins, d.packs = reg, infos
	d.mu.Unlock()
	return infos
}

func loadPacksFrom(dir string) (*plugins.Registry, []packInfo) {
	reg := plugins.NewRegistry()
	infos := []packInfo{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		// A project with no packs directory is the normal case, not a problem.
		if !os.IsNotExist(err) {
			log.Printf("packs: cannot read %s: %v", dir, err)
		}
		return reg, infos
	}

	for _, e := range entries {
		// A pack is a directory. A stray file next to them — a zip someone has
		// not unpacked, a .DS_Store — is not an error, it is just not a pack.
		if !e.IsDir() {
			continue
		}
		infos = append(infos, loadOnePack(reg, filepath.Join(dir, e.Name()), e.Name()))
	}

	// Sorted by name so the settings panel does not rearrange itself between
	// launches, the same reason Registry.Kinds sorts.
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return reg, infos
}

// loadOnePack loads and installs a single pack directory, returning what to
// tell the user either way.
func loadOnePack(reg *plugins.Registry, dir, folder string) packInfo {
	// The folder name stands in until the manifest is read: a pack whose
	// manifest is the broken part still has to be identifiable, and where it
	// lives is the only name it has left.
	info := packInfo{Name: folder, Dir: dir, Capabilities: []string{}, NodeTypes: []string{}}

	pack, err := plugins.Load(dir)
	if err != nil {
		info.Error = err.Error()
		log.Printf("packs: %s was refused: %v", folder, err)
		return info
	}

	info.Name = pack.Manifest.Name
	info.Version = pack.Manifest.Version
	info.Description = pack.Manifest.Description
	info.Author = pack.Manifest.Author
	info.Capabilities = append(info.Capabilities, pack.Manifest.Capabilities...)
	for _, def := range pack.Nodes {
		info.NodeTypes = append(info.NodeTypes, def.Type)
	}

	// Install is where a collision with a built-in or with another pack is
	// caught, and it is all or nothing, so a pack that fails here contributed
	// no nodes at all and the message is the whole story.
	if err := reg.Install(pack); err != nil {
		info.Error = err.Error()
		log.Printf("packs: %s was refused: %v", folder, err)
		return info
	}

	log.Printf("packs: loaded %s %s from %s (%d nodes: %s)",
		info.Name, info.Version, folder, len(info.NodeTypes), strings.Join(info.NodeTypes, ", "))
	return info
}

// pluginRegistry is the registry a run should use. Runs read it while a reload
// may be replacing it, which is the whole reason it is behind the lock.
func (d *daemon) pluginRegistry() *plugins.Registry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.plugins
}

// packList returns a copy, so a handler encoding it cannot be racing a reload
// rewriting the slice underneath it.
func (d *daemon) packList() []packInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]packInfo{}, d.packs...)
}

// packSummary is what the status bar shows. The two numbers are kept apart on
// purpose: a bar reading "2 packs" when one of them contributes nothing is
// worse than no bar, and a refused pack that goes unmentioned is how someone
// spends an afternoon wondering where their node went. So the names are the
// packs that actually loaded, and the failures are counted next to them for the
// panel that can explain them.
func (d *daemon) packSummary() (names []string, failed int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	names = make([]string, 0, len(d.packs))
	for _, p := range d.packs {
		if p.Error != "" {
			failed++
			continue
		}
		names = append(names, p.Name)
	}
	return names, failed
}

// ---------- routes ----------

// listNodes is GET /api/nodes: the whole palette, built-ins and pack nodes in
// one list.
//
// The desktop app no longer carries the built-in set with it. It cannot: half
// the palette only exists on the machine running the engine, and an app that
// hardcoded the other half would show a node this engine has never heard of the
// day the two versions differ. So this is the single source of truth for what
// can be placed on a canvas, and it is served from the same process that would
// have to run it.
func (d *daemon) listNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, engine.Catalogue(d.pluginRegistry()))
}

// listPacks is GET /api/packs: what is installed, including what is broken.
func (d *daemon) listPacks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.packList())
}

// reloadPacks is POST /api/packs/reload: read the folder again and answer with
// the new list. It exists because editing a pack is an edit loop — change a
// line of Lua, run the node, change it again — and restarting the daemon
// between every iteration would close the project the user is working in.
//
// A run already under way keeps the registry it started with: swapping node
// definitions out from under a half-finished graph would produce a run nobody
// could account for afterwards.
func (d *daemon) reloadPacks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.loadPacks())
}
