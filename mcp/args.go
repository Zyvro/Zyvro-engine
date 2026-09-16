package mcp

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// The arguments of a tools/call arrive as a decoded JSON object, so reading one
// is the other half of declaring it in an input schema. These readers live next
// to the catalogue for that reason: WaitArg in particular enforces the very
// bound the wait_seconds schema advertises, and a host with its own copy could
// promise 300 seconds and honour something else.

// StrArg reads a string argument, or "" when it is absent or of another type.
func StrArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// IntArg reads a numeric argument. JSON numbers decode to float64, and some
// clients send integers as strings.
func IntArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}

// WaitArg reads wait_seconds, clamped to the supported range.
func WaitArg(args map[string]any) time.Duration {
	n := IntArg(args, "wait_seconds")
	if n <= 0 {
		return 0
	}
	if n > MaxWaitSeconds {
		n = MaxWaitSeconds
	}
	return time.Duration(n) * time.Second
}
