package plugins

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The budget bounds how long a script runs, not how long a node takes. These
// tests pin both halves, because getting either wrong is bad in a different
// way: a wall clock kills every model call, and a clock that never runs lets a
// hostile pack spin for ever.

// slowLLMPack is a node that calls the model once and returns what it said.
const slowLLMPack = `
return {
  type = "slowcall",
  label = "Slow call",
  inputs = {}, outputs = { "text" }, config = {},
  run = function(ctx)
    return { text = ctx.llm({ prompt = "hello" }) }
  end,
}
`

// spinPack never yields to anything.
const spinPack = `
return {
  type = "spin",
  label = "Spin",
  inputs = {}, outputs = { "text" }, config = {},
  run = function(ctx)
    local n = 0
    while true do n = n + 1 end
  end,
}
`

func clockRegistry(t *testing.T, name, source string) *Registry {
	t.Helper()
	manifest := `{"name":"clocktest","version":"1.0.0","description":"d","author":"a","capabilities":["llm"]}`
	pack := loadOK(t, manifest, map[string]string{name + ".lua": source})
	reg := NewRegistry(testReserved)
	if err := reg.Install(pack); err != nil {
		t.Fatalf("install: %v", err)
	}
	return reg
}

// A provider that takes longer than the script budget is not a runaway script.
// Before the clock existed this failed, which meant every model call slower
// than five seconds failed.
func TestAHostCallSlowerThanTheBudgetDoesNotKillTheNode(t *testing.T) {
	reg := clockRegistry(t, "slowcall", slowLLMPack)

	out, err := reg.Run(context.Background(), "slowcall", HostInput{
		Limits: Limits{Timeout: 300 * time.Millisecond},
		LLM: func(ctx context.Context, req LLMRequest) (string, error) {
			// Four times the budget, spent entirely outside the script.
			time.Sleep(1200 * time.Millisecond)
			return "answered", nil
		},
	})
	if err != nil {
		t.Fatalf("a slow provider killed the node: %v", err)
	}
	if out.Value["text"] != "answered" {
		t.Fatalf("output = %+v", out.Value)
	}
}

// The guard the budget exists for is untouched.
func TestAScriptThatSpinsStillDies(t *testing.T) {
	reg := clockRegistry(t, "spin", spinPack)

	start := time.Now()
	_, err := reg.Run(context.Background(), "spin", HostInput{
		Limits: Limits{Timeout: 300 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("a script that loops for ever was allowed to finish")
	}
	if !strings.Contains(err.Error(), "ran longer than") {
		t.Fatalf("error does not name the budget: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("it took %s to stop a spinning script", elapsed)
	}
}

// The run's own deadline still stops everything, and says so in its own words
// rather than blaming the script.
func TestTheRunDeadlineStillStopsASlowProvider(t *testing.T) {
	reg := clockRegistry(t, "slowcall", slowLLMPack)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := reg.Run(ctx, "slowcall", HostInput{
		Limits: Limits{Timeout: 10 * time.Second},
		LLM: func(ctx context.Context, req LLMRequest) (string, error) {
			time.Sleep(2 * time.Second)
			return "", ctx.Err()
		},
	})
	if err == nil {
		t.Fatal("the run deadline did not stop the node")
	}
	if strings.Contains(err.Error(), "ran longer than") {
		t.Fatalf("the run's deadline was reported as the script's fault: %v", err)
	}
}

// ---------- the clock itself ----------

func TestTheClockDoesNotCountPausedTime(t *testing.T) {
	c := &scriptClock{}
	start := time.Now()
	c.pause()
	time.Sleep(60 * time.Millisecond)
	c.resume()
	if e := c.elapsed(start); e > 30*time.Millisecond {
		t.Fatalf("elapsed = %s, want the paused time excluded", e)
	}
}

// A host function that calls another must not resume the clock on the way out
// of the inner one.
func TestNestedPausesResumeOnlyOnce(t *testing.T) {
	c := &scriptClock{}
	start := time.Now()
	c.pause()
	c.pause()
	time.Sleep(40 * time.Millisecond)
	c.resume() // inner
	if c.depth != 1 {
		t.Fatalf("depth = %d after the inner call returned", c.depth)
	}
	time.Sleep(40 * time.Millisecond)
	c.resume() // outer
	if e := c.elapsed(start); e > 30*time.Millisecond {
		t.Fatalf("elapsed = %s, want both sleeps excluded", e)
	}
}

// A host that never returns must not let the script budget stand still for
// ever: the call in flight counts as it happens, so the run's own deadline is
// what ends it rather than nothing at all.
func TestACallInFlightIsCountedWhileItRuns(t *testing.T) {
	c := &scriptClock{}
	start := time.Now()
	c.pause()
	time.Sleep(50 * time.Millisecond)
	if e := c.elapsed(start); e > 20*time.Millisecond {
		t.Fatalf("elapsed = %s while a call was in flight, want it excluded", e)
	}
}
