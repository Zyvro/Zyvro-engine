package plugins

import (
	"context"
	"strings"
	"testing"
)

func twoNodePack(t *testing.T, name string) *Pack {
	t.Helper()
	return loadOK(t, `{"name":"`+name+`","version":"1.0.0"}`, map[string]string{
		"zeta.lua": `return { type = "zeta", outputs = { "text" }, run = function() return { text = "z" } end }`,
		"alpha.lua": `return { type = "alpha", category = "Utility", outputs = { "text" },
			run = function() return { text = "a" } end }`,
	})
}

func TestInstallAndLookUp(t *testing.T) {
	r := NewRegistry(testReserved)
	if err := r.Install(twoNodePack(t, "pack-one")); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, ok := r.Kind("alpha"); !ok {
		t.Fatal("alpha was not registered")
	}
	if _, ok := r.Kind("nope"); ok {
		t.Fatal("an unregistered type was found")
	}
	if got := r.Packs(); len(got) != 1 || got[0] != "pack-one" {
		t.Fatalf("packs: %v", got)
	}
}

// TestKindsAreStablyOrdered: the palette must not rearrange itself between
// launches, and Go map iteration is deliberately randomised.
func TestKindsAreStablyOrdered(t *testing.T) {
	r := NewRegistry(testReserved)
	if err := r.Install(twoNodePack(t, "pack-one")); err != nil {
		t.Fatalf("install: %v", err)
	}
	for i := 0; i < 20; i++ {
		kinds := r.Kinds()
		if len(kinds) != 2 || kinds[0].Type != "alpha" || kinds[1].Type != "zeta" {
			t.Fatalf("unstable order on pass %d: %v", i, kinds)
		}
	}
}

// TestKindsReturnsCopies: a caller rendering the palette must not be able to
// reach into a registered definition.
func TestKindsReturnsCopies(t *testing.T) {
	r := NewRegistry(testReserved)
	if err := r.Install(twoNodePack(t, "pack-one")); err != nil {
		t.Fatalf("install: %v", err)
	}
	kinds := r.Kinds()
	kinds[0].Outputs[0] = "tampered"
	if def, _ := r.Kind("alpha"); def.Outputs[0] != "text" {
		t.Fatalf("a caller mutated a registered definition: %v", def.Outputs)
	}
}

func TestInstallRejectsADuplicateType(t *testing.T) {
	r := NewRegistry(testReserved)
	if err := r.Install(twoNodePack(t, "pack-one")); err != nil {
		t.Fatalf("install: %v", err)
	}
	err := r.Install(twoNodePack(t, "pack-two"))
	if err == nil {
		t.Fatal("a second pack claiming the same type was installed")
	}
	if !strings.Contains(err.Error(), "pack-one") {
		t.Fatalf("error did not name the pack that already provides it: %v", err)
	}
}

func TestInstallRejectsTheSamePackTwice(t *testing.T) {
	r := NewRegistry(testReserved)
	if err := r.Install(twoNodePack(t, "pack-one")); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := r.Install(twoNodePack(t, "pack-one")); err == nil {
		t.Fatal("the same pack installed twice")
	}
}

// TestInstallIsAllOrNothing. A pack half-installed because its second node
// collided leaves one type registered from a pack the user was told had failed.
func TestInstallIsAllOrNothing(t *testing.T) {
	r := NewRegistry(testReserved)
	first := loadOK(t, `{"name":"first","version":"1.0.0"}`, map[string]string{
		"zeta.lua": `return { type = "zeta", outputs = { "text" }, run = function() return { text = "z" } end }`,
	})
	if err := r.Install(first); err != nil {
		t.Fatalf("install: %v", err)
	}

	clashing := loadOK(t, `{"name":"second","version":"1.0.0"}`, map[string]string{
		"aaa.lua":  `return { type = "brandnew", outputs = { "text" }, run = function() return { text = "n" } end }`,
		"zeta.lua": `return { type = "zeta", outputs = { "text" }, run = function() return { text = "z" } end }`,
	})
	if err := r.Install(clashing); err == nil {
		t.Fatal("a clashing pack installed")
	}
	if _, ok := r.Kind("brandnew"); ok {
		t.Fatal("half of a refused pack was installed")
	}
	if got := r.Packs(); len(got) != 1 {
		t.Fatalf("a refused pack was recorded: %v", got)
	}
}

// TestInstallRejectsABuiltinType, checked again at install and not only at
// load: a built-in added to the engine after a pack was written would otherwise
// be shadowed by a pack that loaded cleanly last week.
func TestInstallRejectsABuiltinType(t *testing.T) {
	p := loadOK(t, `{"name":"sneak","version":"1.0.0"}`, map[string]string{
		"a.lua": `return { type = "harmless", outputs = { "text" }, run = function() return { text = "x" } end }`,
	})
	p.Nodes[0].Type = "llm" // as if "llm" became a built-in after this pack shipped

	r := NewRegistry(testReserved)
	err := r.Install(p)
	if err == nil {
		t.Fatal("a pack shadowing a built-in was installed")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("error did not say why: %v", err)
	}
}

func TestRunRejectsAnUnknownType(t *testing.T) {
	r := NewRegistry(testReserved)
	_, err := r.Run(context.Background(), "ghost", HostInput{})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected an unknown-type error naming it, got: %v", err)
	}
}

func TestInstallRejectsNil(t *testing.T) {
	if err := NewRegistry(testReserved).Install(nil); err == nil {
		t.Fatal("a nil pack installed")
	}
}

// TestAReservedNameCouldActuallyCollide. A reserved name that no node type
// could ever be named is a guard that refuses nothing, and the list now comes
// from another package, so the shape of what arrives is worth checking once.
func TestAReservedNameCouldActuallyCollide(t *testing.T) {
	for _, typ := range testReserved {
		if !nodeTypeRE.MatchString(typ) {
			t.Fatalf("reserved name %q does not match the node type pattern, so no pack could ever collide with it "+
				"and the guard would be silently useless", typ)
		}
	}
}

// TestReservedIsACopy: a registry hands its reserved list to whoever loads a
// pack for it, and a caller that could edit it could un-reserve "llm".
func TestReservedIsACopy(t *testing.T) {
	r := NewRegistry(testReserved)
	got := r.Reserved()
	if len(got) == 0 {
		t.Fatal("a registry built with reserved names reports none")
	}
	got[0] = "harmless"
	if r.Reserved()[0] == "harmless" {
		t.Fatal("a caller rewrote a registry's reserved list")
	}
	// And the slice the caller passed in is not the one held either.
	mutable := append([]string(nil), testReserved...)
	r2 := NewRegistry(mutable)
	mutable[0] = "harmless"
	if r2.Reserved()[0] == "harmless" {
		t.Fatal("a registry kept the caller's slice, so the caller can still edit it")
	}
}

// TestInstallBuiltinMayTakeTheReservedNames: they are reserved for it. It is
// the one pack loaded and installed that way, and the host decides which pack
// that is at the call site rather than a manifest claiming it.
func TestInstallBuiltinMayTakeTheReservedNames(t *testing.T) {
	p := loadOK(t, `{"name":"bundled","version":"1.0.0"}`, map[string]string{
		"a.lua": `return { type = "harmless", outputs = { "text" }, run = function() return { text = "x" } end }`,
	})
	p.Nodes[0].Type = "llm"

	r := NewRegistry(testReserved)
	if err := r.InstallBuiltin(p); err != nil {
		t.Fatalf("the bundled pack was refused its own reserved name: %v", err)
	}
	if !r.IsBuiltin("llm") {
		t.Fatal("a node from the bundled pack does not report as built-in")
	}

	// And now nothing else may have it, with a message about a built-in rather
	// than about some other pack the user has never heard of.
	other := loadOK(t, `{"name":"sneak","version":"1.0.0"}`, map[string]string{
		"a.lua": `return { type = "alsoharmless", outputs = { "text" }, run = function() return { text = "x" } end }`,
	})
	other.Nodes[0].Type = "llm"
	err := r.Install(other)
	if err == nil {
		t.Fatal("a pack shadowed a bundled node")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("error did not say it was a built-in: %v", err)
	}
	if r.IsBuiltin("nothingLikeThat") {
		t.Fatal("an unregistered type reports as built-in")
	}
}
