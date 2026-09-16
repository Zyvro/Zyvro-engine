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
	r := NewRegistry()
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
	r := NewRegistry()
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
	r := NewRegistry()
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
	r := NewRegistry()
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
	r := NewRegistry()
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
	r := NewRegistry()
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

	r := NewRegistry()
	err := r.Install(p)
	if err == nil {
		t.Fatal("a pack shadowing a built-in was installed")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("error did not say why: %v", err)
	}
}

func TestRunRejectsAnUnknownType(t *testing.T) {
	r := NewRegistry()
	_, err := r.Run(context.Background(), "ghost", HostInput{})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected an unknown-type error naming it, got: %v", err)
	}
}

func TestInstallRejectsNil(t *testing.T) {
	if err := NewRegistry().Install(nil); err == nil {
		t.Fatal("a nil pack installed")
	}
}

// TestBuiltinListMatchesTheEngine is a reminder rather than a check: this
// package cannot import engine, so the list is duplicated and nothing but a
// person keeps it in step. If a built-in is added to
// engine.Runtime.executeWithInput, add it here too.
func TestBuiltinListMatchesTheEngine(t *testing.T) {
	for _, typ := range builtinNodeTypes {
		if !nodeTypeRE.MatchString(typ) {
			t.Fatalf("built-in %q does not match the node type pattern, so no pack could ever collide with it "+
				"and the guard would be silently useless", typ)
		}
	}
	for i := 1; i < len(builtinNodeTypes); i++ {
		if builtinNodeTypes[i-1] >= builtinNodeTypes[i] {
			t.Fatalf("builtinNodeTypes is not sorted at %q: keep it sorted so additions are easy to see",
				builtinNodeTypes[i])
		}
	}
}
