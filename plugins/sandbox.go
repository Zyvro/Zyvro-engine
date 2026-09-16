// Package plugins runs user-authored workflow nodes written in Lua.
//
// A pack is a directory of Lua files someone installs from a store, so a new
// node is a download rather than a release. The node types the engine ships
// with are a pack too — one embedded in the binary — which is what makes a
// built-in something a person can read and fork rather than invisible Go.
//
// That download is the whole problem this package exists to solve. Execution is
// local, but local is not trusted: the author of a pack is a stranger, and the
// machine it runs on belongs to the person who installed it. A publish-time
// check is a self-attestation by exactly the party we are defending against,
// and review only ever arrives after the damage. The sandbox is therefore the
// only real control, and the promise it has to keep is narrow: the worst a
// hostile pack can do is burn CPU, spend model quota and return wrong answers.
// Reading a file it was not given, reaching the network, or touching the
// process it runs inside are all outside that promise, and every function in
// this file exists to keep one of them out.
package plugins

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

// Limits is the whole budget one script gets. Everything a script can spend
// without asking the host — time, stack, memory, log space — is named here, so
// there is one place to read to know what a pack is allowed to consume, and one
// place for a host to tighten if it wants to.
type Limits struct {
	// Timeout is the wall clock a single script may run for. It is enforced by
	// the VM loop, which checks the context between instructions.
	Timeout time.Duration
	// CallStackSize bounds recursion depth. Past it a script gets a Lua "stack
	// overflow" error rather than growing the Go stack until the process dies.
	CallStackSize int
	// RegistrySize and RegistryMaxSize bound the Lua data stack: locals, call
	// arguments and anything unpack pushes. Past the maximum a script gets
	// "registry overflow".
	RegistrySize    int
	RegistryMaxSize int
	// MaxLogBytes caps what print and ctx.log may accumulate in one run. A log
	// nobody bounded is a memory leak with a friendly name.
	MaxLogBytes int
	// MaxLLMCalls is how many calls one node execution may make that spend the
	// user's own provider account: ctx.llm, and every host function behind it
	// that reaches a model — generating an image, editing one, removing a
	// background, describing one, running the agent loop. They share this one
	// budget because they share one bill, and an image call is the expensive
	// end of it. The budget is small and the refusal past it names the number.
	// Zero means the default; a negative value is how the pack loader says
	// "none at all".
	MaxLLMCalls int
	// MaxStringBytes caps what one guarded string function may produce in a
	// single call. string.rep and string.format can turn three tokens of source
	// into gigabytes of allocation inside one Go call, where the VM's
	// per-instruction deadline check never runs.
	MaxStringBytes int
	// MaxPatternInputBytes caps the subject string handed to the Lua pattern
	// functions. Their replace step is quadratic in matches times subject
	// length and, being one Go call, cannot be interrupted: measured here,
	// gsub over a 200 KB subject with 200k matches ran for six seconds after
	// its one-second deadline had already passed. Capping the subject is what
	// keeps that quadratic small enough to sit inside the time budget.
	MaxPatternInputBytes int
	// MaxFileBytes caps one ctx.readFile or ctx.writeFile. The path gate has
	// its own 25 MB ceiling; this is the smaller number a node has to live
	// within, because a node output travels through the graph in memory.
	MaxFileBytes int
	// MaxMemoryBytes is how much the Go heap may grow while a script runs
	// before the run is cancelled. Neither the registry limit nor the deadline
	// bounds heap: a plain `local t={} local i=1 while true do t[i]=i i=i+1 end`
	// allocated 567 MB in one second on the machine this was written on, and
	// the only thing standing between that and the user's swap file is this
	// number. Zero means the default; only a negative value disables the
	// watchdog, so that forgetting to set the field cannot switch it off.
	MaxMemoryBytes int64
}

// DefaultLimits is what a host gets for not choosing. The numbers are meant to
// be generous for an honest node and uncomfortable for a hostile one.
func DefaultLimits() Limits {
	return Limits{
		Timeout:              5 * time.Second,
		CallStackSize:        200,
		RegistrySize:         1024,
		RegistryMaxSize:      64 * 1024,
		MaxLogBytes:          16 << 10,
		MaxLLMCalls:          8,
		MaxStringBytes:       1 << 20,
		MaxPatternInputBytes: 64 << 10,
		MaxFileBytes:         8 << 20,
		MaxMemoryBytes:       128 << 20,
	}
}

// loadLimits is the budget for evaluating a node file to collect its
// definition. It is deliberately meaner than a run's: returning a table is not
// work, so a chunk that needs seconds to do it is not a node definition, it is
// something else wearing one.
func loadLimits() Limits {
	l := DefaultLimits()
	l.Timeout = 2 * time.Second
	// A definition cannot call the model, because ctx does not exist while a
	// definition is being collected. Saying so is better than relying on it.
	l.MaxLLMCalls = -1
	return l
}

// withDefaults fills in whatever a host left at zero. A zero field must never
// mean "unlimited": a caller that forgets to set Timeout would otherwise get a
// sandbox with no deadline at all, which is the one mistake this package cannot
// afford to make quietly.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.Timeout <= 0 {
		l.Timeout = d.Timeout
	}
	if l.CallStackSize <= 0 {
		l.CallStackSize = d.CallStackSize
	}
	if l.RegistrySize <= 0 {
		l.RegistrySize = d.RegistrySize
	}
	if l.RegistryMaxSize <= 0 {
		l.RegistryMaxSize = d.RegistryMaxSize
	}
	if l.MaxLogBytes <= 0 {
		l.MaxLogBytes = d.MaxLogBytes
	}
	if l.MaxStringBytes <= 0 {
		l.MaxStringBytes = d.MaxStringBytes
	}
	if l.MaxPatternInputBytes <= 0 {
		l.MaxPatternInputBytes = d.MaxPatternInputBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxMemoryBytes == 0 {
		l.MaxMemoryBytes = d.MaxMemoryBytes
	}
	if l.MaxMemoryBytes < 0 {
		// The one way to say "no watchdog", and it has to be said on purpose.
		l.MaxMemoryBytes = 0
	}
	if l.MaxLLMCalls == 0 {
		l.MaxLLMCalls = d.MaxLLMCalls
	}
	if l.MaxLLMCalls < 0 {
		// The one way to say "no model calls", and it has to be said on purpose.
		l.MaxLLMCalls = 0
	}
	return l
}

// ---------- the locked state ----------

// removedGlobals are the base library functions that have to go, and why.
//
// The allow-list above is what actually removes them; this list survives as the
// record of what each name does, which is the part a person reviewing this file
// needs and a set difference cannot tell them. Opening the base library is not
// optional — a node author needs pairs, type, tostring,
// pcall — but it arrives carrying the functions that undo the rest of this
// file. load and loadstring compile new chunks, which is how an attacker gets
// code past any inspection of the file on disk. dofile, loadfile, require and
// module read the filesystem. setfenv and getfenv reach the environment of any
// function on the stack, including host closures, which is a way back to
// whatever those closed over. newproxy makes userdata with a metatable, the
// classic first step of a gopher-lua escape. collectgarbage lets a script drive
// the host's garbage collector. print writes to the host's stdout.
var removedGlobals = []string{
	"dofile",
	"loadfile",
	"load",
	"loadstring",
	"require",
	"module",
	"setfenv",
	"getfenv",
	"newproxy",
	"collectgarbage",
	"print",
	// Not in the brief, and not documented by gopher-lua either: _printregs is
	// a debugging hook the base library installs that dumps VM registers to
	// stdout. It is exactly the kind of thing a deny-list misses, so it is
	// named here rather than left to be discovered.
	"_printregs",
	// _GOPHER_LUA_VERSION tells a script which VM it is on, which is only ever
	// useful for choosing an exploit.
	"_GOPHER_LUA_VERSION",
}

// sandboxLibs are the only libraries that get opened. io and os are the
// filesystem and the process; package is the module loader, which is a
// filesystem reader with a search path; debug can read and rewrite any
// function's environment and upvalues, which defeats every other control here;
// coroutine and channel introduce scheduling the host's deadline does not
// follow.
var sandboxLibs = []struct {
	name string
	open lua.LGFunction
}{
	{lua.BaseLibName, lua.OpenBase},
	{lua.StringLibName, lua.OpenString},
	{lua.TabLibName, lua.OpenTable},
	{lua.MathLibName, lua.OpenMath},
}

// openSandboxLibs opens those four and nothing else. It is a function of its
// own so a test can build the same unpruned state this does and check what the
// libraries actually installed.
func openSandboxLibs(L *lua.LState) {
	for _, lib := range sandboxLibs {
		L.Push(L.NewFunction(lib.open))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
}

// allowedGlobals is the complete set of names a node may see at the top level.
//
// It is an allow-list rather than a deny-list, and that is the whole point.
// removedGlobals below is correct against gopher-lua v1.1.2 and against the
// names somebody thought to look for; a version that adds a global would
// reopen the hole silently and every test naming only the old names would
// still pass. Pruning to this set means a new global is absent by default and
// somebody has to decide to add it.
var allowedGlobals = map[string]bool{
	// The environment itself, and the version a node might branch on.
	"_G":       true,
	"_VERSION": true,
	// Control flow and errors.
	"assert": true,
	"error":  true,
	"pcall":  true,
	"xpcall": true,
	// Iteration.
	"ipairs": true,
	"next":   true,
	"pairs":  true,
	"select": true,
	"unpack": true,
	// Types and conversion.
	"tonumber": true,
	"tostring": true,
	"type":     true,
	// Tables, including the raw accessors, which are the only way to work on a
	// table without triggering its metamethods.
	"getmetatable": true,
	"setmetatable": true,
	"rawequal":     true,
	"rawget":       true,
	"rawset":       true,
	// The three libraries, and the host's print.
	"math":   true,
	"string": true,
	"table":  true,
	"print":  true,
}

// allowedLibFuncs is the same treatment for the library tables. string is the
// one that matters — dump lived there — but table and math get it too, because
// a rule applied to one of three places is a rule somebody will forget.
var allowedLibFuncs = map[string]map[string]bool{
	"string": {
		"byte": true, "char": true, "find": true, "format": true,
		"gfind": true, "gmatch": true, "gsub": true, "len": true,
		"lower": true, "match": true, "rep": true, "reverse": true,
		"sub": true, "upper": true,
		// The string metatable's __index is the library table itself, which is
		// what makes ("x"):upper() work.
		"__index": true,
	},
	"table": {
		"concat": true, "getn": true, "insert": true,
		"maxn": true, "remove": true, "sort": true,
	},
	"math": {
		"abs": true, "acos": true, "asin": true, "atan": true, "atan2": true,
		"ceil": true, "cos": true, "cosh": true, "deg": true, "exp": true,
		"floor": true, "fmod": true, "frexp": true, "huge": true, "ldexp": true,
		"log": true, "log10": true, "max": true, "min": true,
		"mod": true, "modf": true, "pi": true,
		"pow": true, "rad": true, "random": true, "randomseed": true,
		"sin": true, "sinh": true, "sqrt": true, "tan": true, "tanh": true,
	},
}

// prune deletes every key of a table that is not on an allow-list, and returns
// what it deleted so a caller can say what it did.
func prune(t *lua.LTable, allowed map[string]bool) []string {
	if t == nil {
		return nil
	}
	var doomed []lua.LValue
	var names []string
	t.ForEach(func(k, _ lua.LValue) {
		name, ok := k.(lua.LString)
		if ok && allowed[string(name)] {
			return
		}
		// A non-string key in one of these tables is not something any library
		// puts there, so it goes too rather than being trusted for being odd.
		doomed = append(doomed, k)
		names = append(names, k.String())
	})
	// Deleted after the walk, not during it: mutating a table while iterating
	// it is undefined in Lua and unkind in Go.
	for _, k := range doomed {
		t.RawSet(k, lua.LNil)
	}
	return names
}

// logRegistryKey is where the per-run log lives. The Lua registry is not
// reachable from Lua without the debug or package libraries, neither of which
// this sandbox opens, so it is the right place to keep host state that the
// script must not be able to read or replace.
const logRegistryKey = "zyvro.runlog"

// clockRegistryKey keeps the script clock beside the log, out of the script's
// reach for the same reason: a node that could pause its own budget would have
// no budget.
const clockRegistryKey = "zyvro.scriptclock"

// newState builds a Lua state a stranger's code can be run in.
//
// The state is single use. Nothing is shared between runs — not the globals,
// not the string metatable, not a cached closure — because a script that
// mutates its environment must not be able to leave anything behind for the
// next one.
func newState(ctx context.Context, limits Limits) *lua.LState {
	limits = limits.withDefaults()

	L := lua.NewState(lua.Options{
		SkipOpenLibs:    true,
		CallStackSize:   limits.CallStackSize,
		RegistrySize:    limits.RegistrySize,
		RegistryMaxSize: limits.RegistryMaxSize,
	})

	openSandboxLibs(L)

	// Named first, so that the reason each of these is dangerous is recorded
	// against the name rather than lost in a set difference. The prune below
	// would catch every one of them anyway; this pass is documentation that
	// also happens to execute.
	for _, name := range removedGlobals {
		L.SetGlobal(name, lua.LNil)
	}

	// And then everything else that is not on the allow-list. This is the line
	// that has to hold when gopher-lua is next upgraded: whatever a new version
	// installs, a node author does not get it until someone puts it in
	// allowedGlobals on purpose.
	globals, _ := L.Get(lua.GlobalsIndex).(*lua.LTable)
	prune(globals, allowedGlobals)
	for name, allowed := range allowedLibFuncs {
		lib, _ := L.GetGlobal(name).(*lua.LTable)
		prune(lib, allowed)
	}

	if str, ok := L.GetGlobal("string").(*lua.LTable); ok {
		// string.dump serialises a function to bytecode. It is half of the
		// bytecode round trip whose other half — loading it — is refused in
		// compileChunk, and there is no reason a node needs either. The prune
		// above has already taken it; this says why it is not on the list.
		str.RawSetString("dump", lua.LNil)
		hardenStringLib(L, str, limits)
	}

	log := newRunLog(limits.MaxLogBytes)
	ud := L.NewUserData()
	ud.Value = log
	L.SetField(L.Get(lua.RegistryIndex), logRegistryKey, ud)

	// print is replaced rather than merely removed. A node author will reach
	// for it the first time something does not work, and taking away their only
	// debugging tool without saying so is cruel; this one writes where the host
	// can show it back to them.
	L.SetGlobal("print", L.NewFunction(luaPrint))

	// SetContext is what makes `while true do end` survivable: gopher-lua
	// checks ctx.Done between VM instructions, so a deadline actually stops a
	// running script instead of politely asking it to stop.
	L.SetContext(ctx)
	return L
}

// stateLog returns the log a state accumulates. It never returns nil, so a
// caller does not have to care whether the state came from newState.
func stateLog(L *lua.LState) *runLog {
	if ud, ok := L.GetField(L.Get(lua.RegistryIndex), logRegistryKey).(*lua.LUserData); ok {
		if log, ok := ud.Value.(*runLog); ok {
			return log
		}
	}
	return newRunLog(DefaultLimits().MaxLogBytes)
}

func luaPrint(L *lua.LState) int {
	log := stateLog(L)
	parts := make([]string, 0, L.GetTop())
	for i := 1; i <= L.GetTop(); i++ {
		parts = append(parts, L.ToStringMeta(L.Get(i)).String())
	}
	log.append(strings.Join(parts, "\t"))
	return 0
}

// ---------- string library guards ----------

// hardenStringLib wraps the string functions whose cost is set by their
// arguments rather than by the number of instructions the VM runs.
//
// This is the hole the deadline does not cover. gopher-lua checks the context
// between VM instructions, but a single instruction that calls into Go runs to
// completion: string.rep("x", 4e8) is one instruction and 400 MB, and
// gsub over a large subject is one instruction and minutes of quadratic
// copying. Neither can be interrupted, so both are refused before they start.
func hardenStringLib(L *lua.LState, str *lua.LTable, limits Limits) {
	// rep is reimplemented rather than wrapped because the check it needs —
	// the exact output size — is trivial to compute and the function itself is
	// two lines.
	str.RawSetString("rep", L.NewFunction(func(L *lua.LState) int {
		s := L.CheckString(1)
		n := L.CheckInt(2)
		if n <= 0 || len(s) == 0 {
			L.Push(lua.LString(""))
			return 1
		}
		if n > limits.MaxStringBytes || len(s) > limits.MaxStringBytes/n {
			L.RaiseError("string.rep would build %d x %d bytes, over the sandbox limit of %d bytes for one string operation",
				n, len(s), limits.MaxStringBytes)
		}
		L.Push(lua.LString(strings.Repeat(s, n)))
		return 1
	}))

	// format reaches Go's fmt with a format string the script controls, so
	// "%2000000000d" is a two-gigabyte allocation written as fourteen
	// characters. The width and precision fields are the only part of a format
	// string whose cost is unbounded, so they are the only part checked.
	wrap(L, str, "format", func(L *lua.LState) {
		if n, ok := widestFormatField(L.CheckString(1)); ok && n > limits.MaxStringBytes {
			L.RaiseError("string.format: a field %d characters wide is over the sandbox limit of %d bytes for one string operation",
				n, limits.MaxStringBytes)
		}
	})

	// The pattern functions are quadratic in the worst case and run entirely
	// inside one Go call. Capping the subject bounds that quadratic; the cap is
	// on the subject rather than on the pattern because the subject is what the
	// cost is squared in.
	for _, name := range []string{"find", "match", "gmatch", "gfind", "gsub"} {
		name := name
		wrap(L, str, name, func(L *lua.LState) {
			if s, ok := L.Get(1).(lua.LString); ok && len(s) > limits.MaxPatternInputBytes {
				L.RaiseError("string.%s: subject is %d bytes, over the sandbox limit of %d bytes for pattern matching",
					name, len(s), limits.MaxPatternInputBytes)
			}
		})
	}
}

// wrap replaces a library function with one that runs check first and then
// forwards every argument and every result to the original. Forwarding rather
// than reimplementing matters: the guard is about size, and the semantics
// should stay exactly whatever the Lua a node author already knows does.
func wrap(L *lua.LState, tbl *lua.LTable, name string, check func(*lua.LState)) {
	orig := tbl.RawGetString(name)
	if orig == lua.LNil {
		return
	}
	tbl.RawSetString(name, L.NewFunction(func(L *lua.LState) int {
		check(L)
		n := L.GetTop()
		args := make([]lua.LValue, 0, n)
		for i := 1; i <= n; i++ {
			args = append(args, L.Get(i))
		}
		base := L.GetTop()
		L.Push(orig)
		for _, a := range args {
			L.Push(a)
		}
		// Unprotected on purpose: an error raised by the original should
		// propagate to the script exactly as it would have without the wrapper.
		L.Call(len(args), lua.MultRet)
		return L.GetTop() - base
	}))
}

// widestFormatField returns the largest width or precision named in a format
// string. It reads only the digits between a verb's % and its letter, which is
// enough: everything else in a format string costs what its arguments cost, and
// those are already bounded values on the Lua stack.
func widestFormatField(format string) (int, bool) {
	widest, found := 0, false
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		if i < len(format) && format[i] == '%' {
			continue // an escaped percent, not a verb
		}
		n := 0
		digits := false
		for ; i < len(format); i++ {
			c := format[i]
			if c >= '0' && c <= '9' {
				digits = true
				// Saturate instead of overflowing: "%99999999999999999999d" must
				// come out of this loop as something huge, not as something
				// negative that passes the check.
				if n < 1<<40 {
					n = n*10 + int(c-'0')
				}
				continue
			}
			if c == '.' || c == '-' || c == '+' || c == ' ' || c == '#' {
				if digits && n > widest {
					widest, found = n, true
				}
				n, digits = 0, false
				continue
			}
			break // the verb letter, or something that is not a format at all
		}
		if digits && n > widest {
			widest, found = n, true
		}
	}
	return widest, found
}

// ---------- the bounded log ----------

// runLog is what print and ctx.log write to. It is bounded and it is guarded by
// a mutex, because a run whose deadline expired may still be executing inside a
// Go call while the host has already moved on to read what it logged.
type runLog struct {
	mu        sync.Mutex
	max       int
	size      int
	lines     []string
	truncated bool
}

func newRunLog(max int) *runLog {
	if max <= 0 {
		max = DefaultLimits().MaxLogBytes
	}
	return &runLog{max: max}
}

func (l *runLog) append(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.truncated {
		return
	}
	if l.size+len(line) > l.max {
		// One truncation notice and then silence. Appending "truncated" per
		// line would itself be unbounded output.
		l.lines = append(l.lines, "... log truncated at "+fmt.Sprint(l.max)+" bytes")
		l.truncated = true
		return
	}
	l.size += len(line)
	l.lines = append(l.lines, line)
}

// Lines returns a copy of what was logged, safe to read while the script that
// wrote it may still be running.
func (l *runLog) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.lines))
	copy(out, l.lines)
	return out
}

// ---------- compiling ----------

// errBytecode is what a chunk that is not source text gets refused with.
var errBytecode = errors.New("precompiled Lua bytecode is not accepted, only source text")

// luaBytecodeEscape is the first byte of a precompiled Lua chunk.
const luaBytecodeEscape = 0x1b

// compileChunk turns Lua source into a function prototype, or refuses it.
//
// The loader accepts source text only. A chunk beginning with the escape byte
// 0x1b is precompiled bytecode, and loading attacker-controlled bytecode is a
// known way out of a Lua sandbox: the undumper trusts its input, so hand-built
// bytecode can address registers and upvalues the compiler would never emit and
// read whatever the host left in memory there. gopher-lua happens to have no
// undumper at all today, so this check is refusing something that would already
// fail — but it is refusing it by name, at the door, where a future loader or a
// different VM would have to walk past it.
func compileChunk(name string, src []byte) (*lua.FunctionProto, error) {
	if len(src) > 0 && src[0] == luaBytecodeEscape {
		return nil, fmt.Errorf("%s: %w", name, errBytecode)
	}
	chunk, err := parse.Parse(strings.NewReader(string(src)), name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	proto, err := lua.Compile(chunk, name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return proto, nil
}

// ---------- running ----------

// The causes a run is cancelled with, so the caller can tell them apart from
// each other and from the run's own deadline.
var (
	// errScriptTimeout: the script used its budget of its own execution.
	errScriptTimeout = errors.New("script time budget exhausted")
	// errMemoryLimit: the heap watchdog tripped.
	errMemoryLimit = errors.New("memory limit exceeded")
)

// sandboxRun is the only way anything in this package executes Lua.
//
// The state is built, used and closed on one goroutine that nothing else
// touches, and the caller is released when the budget expires whether or not
// that goroutine has noticed. That second half matters: the VM checks the
// deadline between instructions, but a single instruction that calls into Go
// runs to completion, and a host that blocks forever on one is a hostile pack
// hanging the application. The guards in hardenStringLib are what keep those Go
// calls short; this is what keeps the caller from depending on them being right.
func sandboxRun[T any](ctx context.Context, limits Limits, fn func(*lua.LState) (T, error)) (T, []string, error) {
	limits = limits.withDefaults()
	var zero T

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// The budget is script time, not wall time: it stops while the node waits
	// on a host call the runtime made for it. See scriptclock.go — before that
	// distinction existed, every model call slower than the budget killed the
	// node, which is most model calls.
	clock := &scriptClock{}
	deadline, stopDeadline := watchScriptTime(runCtx, clock, limits.Timeout, func() { cancel(errScriptTimeout) })
	defer stopDeadline()

	stopWatchdog := watchHeap(deadline, limits.MaxMemoryBytes, func() { cancel(errMemoryLimit) })
	defer stopWatchdog()

	type result struct {
		value T
		log   []string
		err   error
	}
	// Buffered, so the goroutine can always finish and release the state even
	// when the caller has already given up on it.
	done := make(chan result, 1)

	// The log is created with the state, so it has to be published from inside
	// the goroutine; logRef lets the caller read it after an early return
	// without racing, because runLog is itself mutex-guarded.
	logRef := make(chan *runLog, 1)

	go func() {
		L := newState(deadline, limits)
		attachClock(L, clock)
		log := stateLog(L)
		logRef <- log
		defer L.Close()
		defer func() {
			// gopher-lua panics rather than returning an error for a handful of
			// internal failures, and a panic crossing back into the host is a
			// pack crashing the application.
			if rec := recover(); rec != nil {
				done <- result{log: log.Lines(), err: fmt.Errorf("script failed: %v", rec)}
			}
		}()
		v, err := fn(L)
		done <- result{value: v, log: log.Lines(), err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return zero, r.log, explainError(deadline, limits, r.err)
		}
		return r.value, r.log, nil
	case <-deadline.Done():
		var log []string
		select {
		case l := <-logRef:
			log = l.Lines()
		default:
		}
		return zero, log, explainError(deadline, limits, deadline.Err())
	}
}

// explainError turns whatever came back into something a node author can act
// on. A Lua script killed by the deadline reports "context deadline exceeded",
// which is true and useless; the budget it blew is the part worth saying.
func explainError(ctx context.Context, limits Limits, err error) error {
	if err == nil {
		return nil
	}
	if cause := context.Cause(ctx); errors.Is(cause, errMemoryLimit) {
		return fmt.Errorf("script used more than %d MB of memory and was stopped", limits.MaxMemoryBytes>>20)
	}
	if errors.Is(context.Cause(ctx), errScriptTimeout) {
		return fmt.Errorf("script ran longer than %s and was stopped", limits.Timeout)
	}
	if ctx.Err() != nil || strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		// The run's own deadline, not the script budget: the whole execution
		// was told to stop, and saying the script was too slow would send its
		// author looking in the wrong place.
		return fmt.Errorf("the run was stopped before this node finished")
	}
	return err
}

// watchHeap cancels a run whose allocation the deadline cannot reach.
//
// Neither the registry limit nor the timeout bounds the Go heap: the registry
// is the Lua data stack, and the timeout only stops the script after it has
// already allocated for as long as it was given. A table-growing loop measured
// 567 MB in one second, so on the default five-second budget the difference
// between this watchdog and no watchdog is the user's swap file.
//
// It is a heap delta rather than an absolute, and a trip is confirmed with a
// forced collection before anything is cancelled, so short-lived garbage — a
// string-concatenation loop, which produces a great deal of it — does not kill
// an honest node. It reads a process-wide number, so a host allocating heavily
// on other goroutines can make a run look worse than it is; cancelling an
// innocent run with a clear error is the side to err on.
func watchHeap(ctx context.Context, max int64, trip func()) (stop func()) {
	if max <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		base := heapBytes()
		lastGC := time.Time{}
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if heapBytes()-base <= max {
					continue
				}
				// Most of an overshoot is usually garbage. Collect once, at
				// most a few times a second, and only cancel if the bytes are
				// still there afterwards — if they are, they are live, and
				// something in the script is holding them.
				if time.Since(lastGC) < 250*time.Millisecond {
					continue
				}
				lastGC = time.Now()
				runtime.GC()
				if heapBytes()-base > max {
					trip()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// heapBytes reports live heap object bytes. runtime/metrics rather than
// runtime.ReadMemStats because this is polled twenty times a second while a
// node runs and ReadMemStats stops the world to answer.
func heapBytes() int64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return int64(sample[0].Value.Uint64())
}
