package plugins

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// runScript compiles and runs a chunk the way a node file is run: through
// compileChunk and sandboxRun, so a test exercises the real door rather than
// a convenience one.
func runScript(t *testing.T, limits Limits, src string) ([]string, error) {
	t.Helper()
	proto, err := compileChunk("test.lua", []byte(src))
	if err != nil {
		return nil, err
	}
	_, log, err := sandboxRun(context.Background(), limits, func(L *lua.LState) (lua.LValue, error) {
		if err := L.CallByParam(lua.P{Fn: L.NewFunctionFromProto(proto), NRet: 0, Protect: true}); err != nil {
			return nil, err
		}
		return lua.LNil, nil
	})
	return log, err
}

func mustRun(t *testing.T, src string) []string {
	t.Helper()
	log, err := runScript(t, DefaultLimits(), src)
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	return log
}

// TestRemovedGlobalsAreNil is the assertion the whole package rests on. Each
// name here is a documented way out of a Lua sandbox, and the list is checked
// through _G as well as by bare name so that removing one view and not the
// other cannot pass.
func TestRemovedGlobalsAreNil(t *testing.T) {
	mustRun(t, `
		local gone = {
			"dofile", "loadfile", "load", "loadstring", "require", "module",
			"setfenv", "getfenv", "newproxy", "collectgarbage", "_printregs",
		}
		for _, name in ipairs(gone) do
			assert(_G[name] == nil, name .. " is still reachable through _G")
		end
		assert(dofile == nil, "dofile")
		assert(loadfile == nil, "loadfile")
		assert(load == nil, "load")
		assert(loadstring == nil, "loadstring")
		assert(require == nil, "require")
		assert(module == nil, "module")
		assert(setfenv == nil, "setfenv")
		assert(getfenv == nil, "getfenv")
		assert(newproxy == nil, "newproxy")
		assert(collectgarbage == nil, "collectgarbage")
		assert(string.dump == nil, "string.dump")
		assert(("x").dump == nil, "string.dump through the string metatable")
	`)
}

// TestPrintIsReplacedNotRemoved: the host function has to be there, because a
// node author reaches for print the moment something does not work.
func TestPrintIsReplacedNotRemoved(t *testing.T) {
	log := mustRun(t, `print("hello", 42)`)
	if len(log) != 1 || log[0] != "hello\t42" {
		t.Fatalf("print did not reach the log: %#v", log)
	}
}

func TestLogIsBounded(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxLogBytes = 64
	log, err := runScript(t, limits, `for i = 1, 1000 do print("aaaaaaaaaaaaaaaaaaaa") end`)
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	if len(log) > 8 {
		t.Fatalf("log was not bounded: %d lines", len(log))
	}
	if !strings.Contains(log[len(log)-1], "truncated") {
		t.Fatalf("log did not say it was truncated: %#v", log)
	}
}

// TestDangerousLibrariesAreAbsent. io and os are the filesystem and the
// process; package is a filesystem reader with a search path; debug can rewrite
// any function's environment and upvalues, which defeats everything else here;
// coroutine and channel introduce scheduling the deadline does not follow.
func TestDangerousLibrariesAreAbsent(t *testing.T) {
	mustRun(t, `
		for _, name in ipairs({"os", "io", "package", "debug", "coroutine", "channel"}) do
			assert(_G[name] == nil, name .. " library is open in the sandbox")
		end
	`)
}

// TestInfiniteLoopIsKilledByTheDeadline. The test's own duration is the point:
// if this ever stops finishing quickly, the VM has stopped checking the context
// and every other guarantee in this package is worth less.
func TestInfiniteLoopIsKilledByTheDeadline(t *testing.T) {
	limits := DefaultLimits()
	limits.Timeout = 200 * time.Millisecond

	start := time.Now()
	_, err := runScript(t, limits, `while true do end`)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an endless loop returned without an error")
	}
	if !strings.Contains(err.Error(), "ran longer than") {
		t.Fatalf("error did not name the time budget: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the deadline took %s to take effect", elapsed)
	}
}

// TestPcallCannotSwallowTheDeadline. pcall catches the error the VM raises when
// the context expires, so a script could try to survive by looping inside one.
// It cannot: the check runs before every instruction, so the next one raises
// again.
func TestPcallCannotSwallowTheDeadline(t *testing.T) {
	limits := DefaultLimits()
	limits.Timeout = 200 * time.Millisecond

	start := time.Now()
	_, err := runScript(t, limits, `
		while true do
			pcall(function() while true do end end)
		end
	`)
	if err == nil {
		t.Fatal("a loop wrapping pcall around an endless loop returned without an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("pcall delayed the deadline by %s", elapsed)
	}
}

// TestUnboundedStackGrowthHitsTheRegistryLimit. unpack pushes one value per
// table entry onto the Lua data stack, which is the registry; a script that
// grows it without bound is stopped by the limit rather than by the machine.
func TestUnboundedStackGrowthHitsTheRegistryLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.RegistrySize = 256
	limits.RegistryMaxSize = 2048

	_, err := runScript(t, limits, `
		local t = {}
		for i = 1, 100000 do t[i] = i end
		return unpack(t)
	`)
	if err == nil {
		t.Fatal("unbounded stack growth was allowed")
	}
	if !strings.Contains(err.Error(), "registry overflow") {
		t.Fatalf("expected a registry overflow, got: %v", err)
	}
}

// TestUnboundedRecursionHitsTheCallStackLimit.
func TestUnboundedRecursionHitsTheCallStackLimit(t *testing.T) {
	_, err := runScript(t, DefaultLimits(), `
		local function f(n) return 1 + f(n + 1) end
		f(1)
	`)
	if err == nil {
		t.Fatal("unbounded recursion was allowed")
	}
	if !strings.Contains(err.Error(), "stack overflow") {
		t.Fatalf("expected a stack overflow, got: %v", err)
	}
}

// TestUnboundedHeapAllocationIsStopped. Neither the deadline nor the registry
// limit bounds the Go heap: a table-growing loop measured 567 MB in one second
// while writing this. The watchdog is the thing that keeps that from becoming
// the user's swap file.
func TestUnboundedHeapAllocationIsStopped(t *testing.T) {
	limits := DefaultLimits()
	limits.Timeout = 30 * time.Second // so it is the memory limit that fires, not the clock
	limits.MaxMemoryBytes = 24 << 20

	start := time.Now()
	_, err := runScript(t, limits, `
		local t = {}
		local i = 1
		while true do t[i] = i i = i + 1 end
	`)
	if err == nil {
		t.Fatal("unbounded allocation was allowed")
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Fatalf("expected the memory limit, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the memory watchdog took %s to take effect", elapsed)
	}
}

// TestGarbageDoesNotTripTheMemoryWatchdog. A string-building loop allocates far
// more than the limit and keeps almost none of it — roughly 160 MB of garbage
// against a 24 MB ceiling and an 80 KB live string. Killing that node would be
// a false positive on ordinary work, which is why the watchdog collects before
// it decides.
//
// The time budget here is generous on purpose: under the race detector this
// loop is slow, and a timeout would look like the failure this test is for.
func TestGarbageDoesNotTripTheMemoryWatchdog(t *testing.T) {
	limits := DefaultLimits()
	limits.Timeout = 30 * time.Second
	limits.MaxMemoryBytes = 24 << 20

	_, err := runScript(t, limits, `
		local s = ""
		for i = 1, 4000 do s = s .. "xxxxxxxxxxxxxxxxxxxx" end
	`)
	if err != nil {
		t.Fatalf("an ordinary string loop was killed: %v", err)
	}
}

// TestPrecompiledBytecodeIsRefused. Loading attacker-controlled bytecode is a
// known way out of a Lua sandbox, because the undumper trusts its input.
func TestPrecompiledBytecodeIsRefused(t *testing.T) {
	_, err := compileChunk("evil.lua", []byte{0x1b, 'L', 'u', 'a', 0x51})
	if err == nil {
		t.Fatal("a bytecode chunk was accepted")
	}
	if !strings.Contains(err.Error(), "bytecode") {
		t.Fatalf("error did not name the problem: %v", err)
	}
}

func TestSourceStartingWithTextIsAccepted(t *testing.T) {
	if _, err := compileChunk("ok.lua", []byte("return 1")); err != nil {
		t.Fatalf("plain source was refused: %v", err)
	}
}

// TestStringRepIsCapped. string.rep is one VM instruction and an unbounded
// allocation, and the per-instruction deadline check never runs inside it.
func TestStringRepIsCapped(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStringBytes = 1 << 16

	_, err := runScript(t, limits, `local s = ("x"):rep(400000000)`)
	if err == nil {
		t.Fatal("string.rep built an unbounded string")
	}
	if !strings.Contains(err.Error(), "string.rep") {
		t.Fatalf("error did not name string.rep: %v", err)
	}

	// The guard must not break the function for honest use.
	mustRun(t, `assert(("ab"):rep(3) == "ababab")`)
	mustRun(t, `assert(("ab"):rep(0) == "")`)
	mustRun(t, `assert(string.rep("ab", -1) == "")`)
}

// TestStringFormatWidthIsCapped. "%2000000000d" is a two-gigabyte allocation
// written as fourteen characters, and the format string belongs to the script.
func TestStringFormatWidthIsCapped(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStringBytes = 1 << 16

	_, err := runScript(t, limits, `local s = string.format("%2000000000d", 1)`)
	if err == nil {
		t.Fatal("string.format built an unbounded string")
	}
	if !strings.Contains(err.Error(), "string.format") {
		t.Fatalf("error did not name string.format: %v", err)
	}

	mustRun(t, `assert(string.format("%05.2f|%s", 1.5, "x") == "01.50|x")`)
	mustRun(t, `assert(string.format("100%% of %d", 3) == "100% of 3")`)
}

func TestWidestFormatField(t *testing.T) {
	cases := []struct {
		format string
		want   int
		found  bool
	}{
		{"%d", 0, false},
		{"%2000000000d", 2000000000, true},
		{"%-10s", 10, true},
		{"%.9999999s", 9999999, true},
		{"%%50d", 0, false},
		{"%99999999999999999999d", 0, true}, // saturates rather than overflowing negative
		{"%5d and %700d", 700, true},
	}
	for _, c := range cases {
		got, found := widestFormatField(c.format)
		if found != c.found {
			t.Fatalf("%q: found = %v, want %v", c.format, found, c.found)
		}
		if c.format == "%99999999999999999999d" {
			if got <= 0 {
				t.Fatalf("%q: saturation produced %d", c.format, got)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("%q: width = %d, want %d", c.format, got, c.want)
		}
	}
}

// TestPatternSubjectIsCapped. The replace step of gsub is quadratic in matches
// times subject length and runs inside one Go call, so it cannot be
// interrupted: measured here, a 200 KB subject ran for six seconds past a
// one-second deadline. Capping the subject is what bounds that.
func TestPatternSubjectIsCapped(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStringBytes = 1 << 20
	limits.MaxPatternInputBytes = 4096
	limits.Timeout = 5 * time.Second

	for _, call := range []string{
		`s:gsub("", "b")`,
		`s:find(".-b")`,
		`s:match(".-b")`,
		`for _ in s:gmatch("a") do end`,
	} {
		start := time.Now()
		_, err := runScript(t, limits, `local s = ("a"):rep(200000) `+call)
		if err == nil {
			t.Fatalf("%s: an oversized subject was accepted", call)
		}
		if !strings.Contains(err.Error(), "pattern matching") {
			t.Fatalf("%s: error did not name the pattern limit: %v", call, err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("%s: refusal took %s, so the work was done anyway", call, elapsed)
		}
	}

	// Still a working pattern library for anything of a sane size.
	mustRun(t, `
		assert(("hello world"):gsub("o", "0") == "hell0 w0rld")
		assert(("hello"):find("ll") == 3)
		assert(("key=value"):match("(%w+)=(%w+)") == "key")
	`)
}

// TestStateIsNotSharedBetweenRuns. A script that rewrites its environment must
// not be able to leave anything behind for the next one.
func TestStateIsNotSharedBetweenRuns(t *testing.T) {
	mustRun(t, `_G.planted = "gotcha" string.format = function() return "hijacked" end`)
	mustRun(t, `
		assert(_G.planted == nil, "a global survived into the next run")
		assert(string.format("%d", 7) == "7", "a rewritten string.format survived into the next run")
	`)
}

// TestSyntaxErrorsAreReportedWithTheFileName.
func TestSyntaxErrorsAreReportedWithTheFileName(t *testing.T) {
	_, err := compileChunk("broken.lua", []byte("this is not lua"))
	if err == nil {
		t.Fatal("broken source compiled")
	}
	if !strings.Contains(err.Error(), "broken.lua") {
		t.Fatalf("error did not name the file: %v", err)
	}
}

func TestZeroLimitsAreNeverUnlimited(t *testing.T) {
	l := Limits{}.withDefaults()
	d := DefaultLimits()
	if l.Timeout != d.Timeout || l.RegistryMaxSize != d.RegistryMaxSize || l.MaxMemoryBytes != d.MaxMemoryBytes {
		t.Fatalf("a zero Limits did not fall back to the defaults: %+v", l)
	}
	if l.MaxLLMCalls != d.MaxLLMCalls {
		t.Fatalf("a zero MaxLLMCalls did not fall back to the default: %+v", l)
	}
	if (Limits{MaxLLMCalls: -1}).withDefaults().MaxLLMCalls != 0 {
		t.Fatal("a negative MaxLLMCalls did not mean none at all")
	}
	if (Limits{MaxMemoryBytes: -1}).withDefaults().MaxMemoryBytes != 0 {
		t.Fatal("a negative MaxMemoryBytes did not disable the watchdog")
	}
	if l.CallStackSize <= 0 || l.MaxPatternInputBytes <= 0 || l.MaxStringBytes <= 0 || l.MaxFileBytes <= 0 {
		t.Fatalf("a zero Limits left a budget unset: %+v", l)
	}
}

// ---------- the allow-list, checked as a property ----------

// namesIn enumerates the keys of a table, which is the only way to ask what is
// actually reachable rather than what somebody remembered to look for.
func namesIn(t *lua.LTable) map[string]bool {
	out := map[string]bool{}
	if t == nil {
		return out
	}
	t.ForEach(func(k, _ lua.LValue) { out[k.String()] = true })
	return out
}

func diff(got, want map[string]bool) (extra, missing []string) {
	for name := range got {
		if !want[name] {
			extra = append(extra, name)
		}
	}
	for name := range want {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	return extra, missing
}

// TestSandboxExposesExactlyTheAllowList is the property the deny-list could not
// state: not "these particular dangerous names are gone" but "nothing is
// reachable except what is on the list". A name that appears here without being
// added to allowedGlobals on purpose fails the test by name.
func TestSandboxExposesExactlyTheAllowList(t *testing.T) {
	_, _, err := sandboxRun(context.Background(), DefaultLimits(), func(L *lua.LState) (lua.LValue, error) {
		globals, _ := L.Get(lua.GlobalsIndex).(*lua.LTable)
		if extra, missing := diff(namesIn(globals), allowedGlobals); len(extra) > 0 || len(missing) > 0 {
			return nil, fmt.Errorf("globals: unexpected %v, missing %v", extra, missing)
		}
		for name, allowed := range allowedLibFuncs {
			lib, _ := L.GetGlobal(name).(*lua.LTable)
			if lib == nil {
				return nil, fmt.Errorf("the %s library is not open", name)
			}
			if extra, missing := diff(namesIn(lib), allowed); len(extra) > 0 || len(missing) > 0 {
				return nil, fmt.Errorf("%s: unexpected %v, missing %v", name, extra, missing)
			}
		}
		return lua.LNil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNoUnreviewedGlobalArrivesWithTheLibraries is the tripwire for a
// dependency bump.
//
// The sandbox prunes to the allow-list, so a new global gopher-lua introduces
// would be safely gone and nobody would ever hear about it — including in the
// cases where a node author should have been given it. This opens the same four
// libraries without pruning and insists that every name they install is one
// somebody has already had an opinion about: on allowedGlobals because a node
// needs it, or on removedGlobals because it is dangerous and the comment there
// says why. Anything else fails here, named, so the decision gets made.
func TestNoUnreviewedGlobalArrivesWithTheLibraries(t *testing.T) {
	reviewed := map[string]bool{}
	for name := range allowedGlobals {
		reviewed[name] = true
	}
	for _, name := range removedGlobals {
		reviewed[name] = true
	}

	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()
	openSandboxLibs(L)

	globals, _ := L.Get(lua.GlobalsIndex).(*lua.LTable)
	if extra, _ := diff(namesIn(globals), reviewed); len(extra) > 0 {
		t.Fatalf("gopher-lua installs globals nobody has reviewed: %v — "+
			"add each to allowedGlobals if a node should have it, or to removedGlobals with a comment saying why not", extra)
	}

	// string is where dump lived, so the same tripwire applies to it.
	stringLib := map[string]bool{"dump": true}
	for name := range allowedLibFuncs["string"] {
		stringLib[name] = true
	}
	lib, _ := L.GetGlobal("string").(*lua.LTable)
	if extra, _ := diff(namesIn(lib), stringLib); len(extra) > 0 {
		t.Fatalf("gopher-lua installs string functions nobody has reviewed: %v", extra)
	}

	for _, name := range []string{"table", "math"} {
		lib, _ := L.GetGlobal(name).(*lua.LTable)
		if extra, _ := diff(namesIn(lib), allowedLibFuncs[name]); len(extra) > 0 {
			t.Fatalf("gopher-lua installs %s functions nobody has reviewed: %v", name, extra)
		}
	}
}

// TestAnUnknownGlobalWouldBePruned proves the prune actually runs, rather than
// the allow-list happening to match what the libraries install.
func TestAnUnknownGlobalWouldBePruned(t *testing.T) {
	if allowedGlobals["definitelyNotAllowed"] {
		t.Fatal("the fixture name is on the allow-list")
	}
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()
	openSandboxLibs(L)
	L.SetGlobal("definitelyNotAllowed", lua.LString("dangerous"))

	globals, _ := L.Get(lua.GlobalsIndex).(*lua.LTable)
	removed := prune(globals, allowedGlobals)

	if !slices.Contains(removed, "definitelyNotAllowed") {
		t.Fatalf("prune did not remove an unknown global: %v", removed)
	}
	if globals.RawGetString("definitelyNotAllowed") != lua.LNil {
		t.Fatal("the unknown global is still reachable")
	}
}
