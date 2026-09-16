package plugins

import (
	"context"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// The timeout in Limits bounds how long a *script* may run, not how long a node
// may take.
//
// The difference is the whole point. The budget exists to stop a hostile pack
// spinning in a loop; it is not a service-level promise about model latency. A
// provider that takes twelve seconds to answer is not a runaway script, and
// before this existed it killed the node anyway — every model call slower than
// five seconds failed, which is most of them.
//
// So the clock stops while the node is blocked in a host call the runtime made
// on its behalf, and runs again the moment control returns to Lua. A script
// that loops forever still dies after its budget of its own execution; a script
// that waits on a model waits as long as the run's own deadline allows.
type scriptClock struct {
	mu sync.Mutex
	// pausedAt is when the current outbound call began, zero when none is.
	pausedAt time.Time
	// depth counts nested calls, because a host function that calls another
	// must not resume the clock when the inner one returns.
	depth int
	// waited is time already spent outside the script.
	waited time.Duration
}

func (c *scriptClock) pause() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.depth++
	if c.depth == 1 {
		c.pausedAt = time.Now()
	}
}

func (c *scriptClock) resume() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.depth == 0 {
		return
	}
	c.depth--
	if c.depth == 0 && !c.pausedAt.IsZero() {
		c.waited += time.Since(c.pausedAt)
		c.pausedAt = time.Time{}
	}
}

// elapsed is how long the script itself has run.
func (c *scriptClock) elapsed(since time.Time) time.Duration {
	if c == nil {
		return time.Since(since)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	waited := c.waited
	if c.depth > 0 && !c.pausedAt.IsZero() {
		// Count the call in flight, or a host that never returns would let the
		// script budget stand still for ever.
		waited += time.Since(c.pausedAt)
	}
	return time.Since(since) - waited
}

// watchScriptTime cancels a run once the script itself has used its budget.
//
// A ticker rather than a timer, because the budget is not a fixed point in the
// future: every host call moves it. The interval is a tenth of the budget, so
// the overshoot is bounded by that and a spinning script still dies promptly.
func watchScriptTime(ctx context.Context, clock *scriptClock, budget time.Duration, onExpiry func()) (context.Context, func()) {
	out, cancel := context.WithCancel(ctx)
	start := time.Now()
	interval := budget / 10
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-out.Done():
				return
			case <-tick.C:
				if clock.elapsed(start) >= budget {
					onExpiry()
					cancel()
					return
				}
			}
		}
	}()
	return out, func() { close(stop); cancel() }
}

// offScript runs a function with the script clock paused, and is how every call
// that leaves this process is wrapped.
func offScript[T any](c *scriptClock, fn func() (T, error)) (T, error) {
	c.pause()
	defer c.resume()
	return fn()
}

// attachClock puts the clock where the host can find it, in the Lua registry —
// which no script can reach, since this sandbox opens neither the debug nor the
// package library. A node able to pause its own budget would have none.
func attachClock(L *lua.LState, c *scriptClock) {
	ud := L.NewUserData()
	ud.Value = c
	L.SetField(L.Get(lua.RegistryIndex), clockRegistryKey, ud)
}

// stateClock returns the clock a state runs under. A nil clock is a working
// clock that never pauses, so a caller does not have to care whether the state
// came from sandboxRun.
func stateClock(L *lua.LState) *scriptClock {
	if ud, ok := L.GetField(L.Get(lua.RegistryIndex), clockRegistryKey).(*lua.LUserData); ok {
		if c, ok := ud.Value.(*scriptClock); ok {
			return c
		}
	}
	return nil
}
