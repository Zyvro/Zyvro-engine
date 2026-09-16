package engine

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/Zyvro/Zyvro-engine/plugins"
)

// Three lists have to agree about what a built-in node is, and none of them can
// be derived from the others:
//
//   - the switch in executeWithInput, which is what actually runs one;
//   - builtinKinds in nodes.go, which is what the builder may place;
//   - plugins.builtinNodeTypes, which is what a pack may not shadow.
//
// The third is the dangerous one. It lives in plugins because plugins must not
// import engine, and a name missing from it is a name a pack may take: a pack
// defining "llm" would be a very good attack, and it would have loaded cleanly.
// Nothing keeps a hand-maintained mirror in step except a test that reads both
// sides, which is what this file is. It lives in engine because engine is the
// package allowed to import plugins, the direction that does not cycle.
//
// Reading the source rather than a variable is deliberate: plugins.builtinNodeTypes
// is unexported and stays that way, and the switch is a switch and cannot be
// enumerated at runtime at all. So both are read as Go syntax, and a test that
// cannot find either list fails rather than quietly checking nothing.

// ---------- the three lists ----------

// switchNodeTypes returns the case labels of the dispatch switch in
// executeWithInput: the definitive answer to "what can this engine run".
func switchNodeTypes(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "runtime.go", nil, 0)
	if err != nil {
		t.Fatalf("parse runtime.go: %v", err)
	}

	var types []string
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "executeWithInput" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || sw.Tag == nil || exprString(t, fset, sw.Tag) != "in.Node.Type" {
				return true
			}
			found = true
			for _, stmt := range sw.Body.List {
				clause, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range clause.List {
					types = append(types, stringLit(t, expr))
				}
			}
			return false
		})
		return false
	})

	if !found {
		t.Fatal("no switch on in.Node.Type in executeWithInput: this test has lost track of the dispatch and is no longer checking anything")
	}
	sort.Strings(types)
	return types
}

// pluginsMirror returns plugins.builtinNodeTypes, read from its source.
func pluginsMirror(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "plugins", "pack.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var types []string
	found := false
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != "builtinNodeTypes" || len(value.Values) != 1 {
				continue
			}
			lit, ok := value.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatalf("%s: builtinNodeTypes is no longer a literal list", path)
			}
			found = true
			for _, elem := range lit.Elts {
				types = append(types, stringLit(t, elem))
			}
		}
	}

	if !found {
		t.Fatalf("%s: no builtinNodeTypes: either it was renamed, or a pack can now shadow a built-in and nothing says so", path)
	}
	sort.Strings(types)
	return types
}

func exprString(t *testing.T, fset *token.FileSet, expr ast.Expr) string {
	t.Helper()
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, expr); err != nil {
		t.Fatalf("print expression: %v", err)
	}
	return buf.String()
}

func stringLit(t *testing.T, expr ast.Expr) string {
	t.Helper()
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("expected a string literal node type, got %T", expr)
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatalf("unquote %s: %v", lit.Value, err)
	}
	return s
}

// diff reports what is in want but not got, and the other way round, which is
// the only form of this failure anybody can act on.
func diff(got, want []string) (missing, extra []string) {
	inGot := map[string]bool{}
	for _, s := range got {
		inGot[s] = true
	}
	inWant := map[string]bool{}
	for _, s := range want {
		inWant[s] = true
	}
	for _, s := range want {
		if !inGot[s] {
			missing = append(missing, s)
		}
	}
	for _, s := range got {
		if !inWant[s] {
			extra = append(extra, s)
		}
	}
	return missing, extra
}

// ---------- the agreements ----------

// TestPluginsMirrorsTheEngineSwitchExactly is the test the sandbox author asked
// for. A built-in added to the engine and not to plugins.builtinNodeTypes is a
// built-in a pack may shadow, and this is what stops that being discovered by
// the pack that does it.
func TestPluginsMirrorsTheEngineSwitchExactly(t *testing.T) {
	engineTypes := switchNodeTypes(t)
	mirror := pluginsMirror(t)

	missing, extra := diff(mirror, engineTypes)
	for _, name := range missing {
		t.Errorf("%q is a built-in the engine runs but plugins.builtinNodeTypes does not list: a pack can shadow it. Add it to builtinNodeTypes in plugins/pack.go.", name)
	}
	for _, name := range extra {
		t.Errorf("%q is listed in plugins.builtinNodeTypes but the engine has no case for it: packs are being refused a name nothing uses. Remove it from builtinNodeTypes in plugins/pack.go.", name)
	}
}

// TestPluginsRefusesEveryBuiltinByName checks the same agreement through the
// behaviour rather than the source, so that reading the list the wrong way
// cannot make this file pass while a pack named "llm" installs happily.
func TestPluginsRefusesEveryBuiltinByName(t *testing.T) {
	for _, nodeType := range switchNodeTypes(t) {
		t.Run(nodeType, func(t *testing.T) {
			_, err := plugins.Load(packDefining(t, nodeType))
			if err == nil {
				t.Fatalf("a pack was allowed to define %q, which is a built-in", nodeType)
			}
		})
	}
}

// packDefining writes a minimal pack whose one node claims a node type.
func packDefining(t *testing.T, nodeType string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nodes"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := []byte(`{"name":"shadow","version":"1.0.0"}`)
	if err := os.WriteFile(filepath.Join(dir, "zyvro-pack.json"), manifest, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	src := fmt.Sprintf("return { type = %q, outputs = { \"text\" }, run = function() return { text = \"x\" } end }", nodeType)
	if err := os.WriteFile(filepath.Join(dir, "nodes", "node.lua"), []byte(src), 0o644); err != nil {
		t.Fatalf("write node: %v", err)
	}
	return dir
}

// TestCatalogueCoversEveryNodeTheEngineCanRun is the other direction of the
// same agreement, and it matters now that the desktop learns the palette from
// GET /api/nodes instead of carrying its own copy: a built-in missing from
// builtinKinds does not fail, it simply disappears from the editor, which is
// the kind of bug that gets noticed a release later.
func TestCatalogueCoversEveryNodeTheEngineCanRun(t *testing.T) {
	served := []string{}
	for _, k := range BuiltinKinds() {
		served = append(served, k.Type)
	}
	// A disabled node is deliberately absent from the palette: nobody should be
	// able to place a node that refuses to run. The case for it stays so an old
	// workflow still gets the real explanation instead of "unknown node type",
	// so it is expected here and named rather than quietly tolerated.
	served = append(served, disabledNodeTypes...)

	missing, extra := diff(served, switchNodeTypes(t))
	for _, name := range missing {
		t.Errorf("%q is a node the engine runs but the palette does not offer: it has disappeared from the editor. Add it to builtinKinds in engine/nodes.go, or to disabledNodeTypes if that is on purpose.", name)
	}
	for _, name := range extra {
		t.Errorf("%q is offered in the palette but executeWithInput has no case for it: placing it would fail at run time. Remove it from engine/nodes.go.", name)
	}
}

// TestBuiltinNodeTypesIsTheWholeReservedSet: BuiltinNodeTypes is what a caller
// outside this package asks when it wants to know which names are taken, so it
// has to be the palette and the disabled cases together, not just one of them.
func TestBuiltinNodeTypesIsTheWholeReservedSet(t *testing.T) {
	missing, extra := diff(BuiltinNodeTypes(), switchNodeTypes(t))
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("BuiltinNodeTypes disagrees with the dispatch switch: missing %v, extra %v", missing, extra)
	}
}

// TestLocalOnlyFlagAgreesWithLocalOnlyNodeTypes: the flag is what the builder
// badges and what the hosted server refuses a graph for, and the two have to be
// the same set or one of them is lying to somebody.
func TestLocalOnlyFlagAgreesWithLocalOnlyNodeTypes(t *testing.T) {
	flagged := []string{}
	for _, k := range BuiltinKinds() {
		if k.LocalOnly {
			flagged = append(flagged, k.Type)
		}
	}
	missing, extra := diff(flagged, LocalOnlyNodeTypes)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("localOnly in the palette disagrees with LocalOnlyNodeTypes: missing %v, extra %v", missing, extra)
	}
}

// TestCatalogueIsCopiedPerCall: a caller renders this and must not be able to
// reach back into the table every later caller is served from.
func TestCatalogueIsCopiedPerCall(t *testing.T) {
	first := BuiltinKinds()
	first[0].Label = "tampered"
	first[0].Defaults["injected"] = true
	first[0].Outputs[0] = "tampered"

	second := BuiltinKinds()
	if second[0].Label == "tampered" || second[0].Outputs[0] == "tampered" {
		t.Error("a caller mutated the shared palette")
	}
	if _, ok := second[0].Defaults["injected"]; ok {
		t.Error("a caller added a default to the shared palette")
	}
}
