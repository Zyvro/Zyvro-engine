package plugins

import (
	"encoding/json"
	"fmt"
	"sort"

	lua "github.com/yuin/gopher-lua"
)

// The two directions a value can cross the sandbox boundary. Both are bounded,
// and both are bounded for the same reason: the shapes on either side are
// recursive, and recursive conversion driven by untrusted data is how a
// converter turns into a stack overflow or an allocation loop.

const (
	// maxValueDepth is how deeply nested a value may be. JSON from a workflow
	// is a handful of levels; a Lua table can reference itself, which is
	// infinitely many.
	maxValueDepth = 32
	// maxValueNodes is how many table entries one conversion may produce. A
	// script that builds a million-entry table inside its budget must not be
	// able to spend the host's memory again on the way out.
	maxValueNodes = 100000
)

// toLua converts a host value into a Lua value. Anything it does not recognise
// becomes nil rather than an opaque userdata: handing a script a Go value it
// can hold but not inspect is how host objects leak into a sandbox.
func toLua(L *lua.LState, v any, depth int) lua.LValue {
	if depth > maxValueDepth {
		return lua.LNil
	}
	switch t := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(t)
	case string:
		return lua.LString(t)
	case float64:
		return lua.LNumber(t)
	case float32:
		return lua.LNumber(t)
	case int:
		return lua.LNumber(t)
	case int32:
		return lua.LNumber(t)
	case int64:
		return lua.LNumber(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return lua.LString(t.String())
		}
		return lua.LNumber(f)
	case []byte:
		return lua.LString(t)
	case map[string]any:
		tbl := L.NewTable()
		// Sorted, so the same config always builds the same table. Map order in
		// Go is randomised, and a node whose behaviour depends on it would fail
		// one run in ten for reasons nobody could reproduce.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			tbl.RawSetString(k, toLua(L, t[k], depth+1))
		}
		return tbl
	case []any:
		tbl := L.NewTable()
		for _, item := range t {
			tbl.Append(toLua(L, item, depth+1))
		}
		return tbl
	case []string:
		tbl := L.NewTable()
		for _, item := range t {
			tbl.Append(lua.LString(item))
		}
		return tbl
	default:
		return lua.LNil
	}
}

// fromLua converts a Lua value back into something the engine can carry through
// a graph and serialise as JSON. budget is decremented across the whole
// conversion, not per level, so a wide table costs as much as a deep one.
func fromLua(v lua.LValue, depth int, budget *int) (any, error) {
	if depth > maxValueDepth {
		return nil, fmt.Errorf("value is nested more than %d levels deep", maxValueDepth)
	}
	switch t := v.(type) {
	case *lua.LNilType, nil:
		return nil, nil
	case lua.LBool:
		return bool(t), nil
	case lua.LString:
		return string(t), nil
	case lua.LNumber:
		return float64(t), nil
	case *lua.LTable:
		return tableFromLua(t, depth, budget)
	default:
		// Functions, userdata and threads have no JSON form and no meaning
		// downstream. Refusing beats silently dropping them, because a node
		// returning a function is a node whose author misunderstood something.
		return nil, fmt.Errorf("cannot carry a Lua %s out of a node", v.Type().String())
	}
}

// tableFromLua turns a table into a slice when it looks like a Lua array and a
// map otherwise, which is the shape a workflow's JSON expects on the other side.
func tableFromLua(t *lua.LTable, depth int, budget *int) (any, error) {
	n := t.Len()
	if n > 0 {
		// A table with a sequence part may still carry string keys; only the
		// ones that are purely a sequence become an array.
		onlySequence := true
		t.ForEach(func(k, _ lua.LValue) {
			if num, ok := k.(lua.LNumber); !ok || float64(num) != float64(int(num)) || int(num) < 1 || int(num) > n {
				onlySequence = false
			}
		})
		if onlySequence {
			out := make([]any, 0, n)
			for i := 1; i <= n; i++ {
				*budget--
				if *budget < 0 {
					return nil, fmt.Errorf("value has more than %d entries", maxValueNodes)
				}
				item, err := fromLua(t.RawGetInt(i), depth+1, budget)
				if err != nil {
					return nil, err
				}
				out = append(out, item)
			}
			return out, nil
		}
	}

	out := map[string]any{}
	var err error
	t.ForEach(func(k, val lua.LValue) {
		if err != nil {
			return
		}
		*budget--
		if *budget < 0 {
			err = fmt.Errorf("value has more than %d entries", maxValueNodes)
			return
		}
		key, kerr := tableKey(k)
		if kerr != nil {
			err = kerr
			return
		}
		item, verr := fromLua(val, depth+1, budget)
		if verr != nil {
			err = verr
			return
		}
		out[key] = item
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func tableKey(k lua.LValue) (string, error) {
	switch key := k.(type) {
	case lua.LString:
		return string(key), nil
	case lua.LNumber:
		return fmt.Sprintf("%v", float64(key)), nil
	default:
		return "", fmt.Errorf("table keys must be strings or numbers, got %s", k.Type().String())
	}
}

// luaString reads a string field, treating a missing field and an empty one the
// same way. Node definitions and node results are both read with it, so a
// mistyped field name fails the same way a blank one does.
func luaString(t *lua.LTable, key string) string {
	if s, ok := t.RawGetString(key).(lua.LString); ok {
		return string(s)
	}
	return ""
}

// luaBool reads a boolean field, treating a missing one as false. Only a real
// Lua boolean counts: a node writing toolOnly = "yes" has made a mistake that
// silently reading it as true would hide.
func luaBool(t *lua.LTable, key string) bool {
	b, ok := t.RawGetString(key).(lua.LBool)
	return ok && bool(b)
}

// luaStringList reads a list-of-strings field, refusing anything that is not
// one rather than skipping it: a node declaring inputs = {"text", 3} has a bug
// its author should be told about.
func luaStringList(t *lua.LTable, key string) ([]string, error) {
	v := t.RawGetString(key)
	if v == lua.LNil {
		return nil, nil
	}
	tbl, ok := v.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", key)
	}
	out := make([]string, 0, tbl.Len())
	for i := 1; i <= tbl.Len(); i++ {
		s, ok := tbl.RawGetInt(i).(lua.LString)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string", key, i)
		}
		out = append(out, string(s))
	}
	return out, nil
}
