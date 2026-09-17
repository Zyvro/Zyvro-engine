package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/engine"
)

// The defaults are part of the contract: a binary that did not go through the
// release script has to admit it rather than name a version it is not.
func TestUnstampedBuildSaysSo(t *testing.T) {
	if Version != "dev" || Commit != "unknown" {
		t.Fatalf("defaults are %q/%q, want dev/unknown", Version, Commit)
	}
	want := fmt.Sprintf("zyvrod dev %s/%s unknown", runtime.GOOS, runtime.GOARCH)
	if got := versionLine(); got != want {
		t.Errorf("versionLine() = %q, want %q", got, want)
	}
}

// --version is built and run for real, because the two things worth knowing —
// that the ldflags land on these variables and that the process exits 0 — are
// exactly the two an in-process test cannot observe.
func TestVersionFlagPrintsAndExitsZero(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "zyvrod-under-test")
	build := exec.Command("go", "build",
		"-ldflags", "-X main.Version=1.4.0 -X main.Commit=deadbee",
		"-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("--version exited non-zero: %v", err)
	}
	want := fmt.Sprintf("zyvrod 1.4.0 %s/%s deadbee\n", runtime.GOOS, runtime.GOARCH)
	if string(out) != want {
		t.Errorf("--version printed %q, want %q", out, want)
	}

	// Without it the daemon still refuses to start without a project, which is
	// the check --version has to run ahead of rather than around.
	noProject := exec.Command(bin)
	if err := noProject.Run(); err == nil {
		t.Error("the daemon started with no --project")
	}
}

// The handshake is the desktop app's first contact with an engine it did not
// build. Every field that was there before this change is still there, and the
// version rides along beside them.
func TestHandshakeCarriesTheVersion(t *testing.T) {
	e := newTestEnv(t)

	path := filepath.Join(t.TempDir(), "handshake.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := e.daemon.writeHandshake(f, 4242); err != nil {
		t.Fatalf("writeHandshake: %v", err)
	}
	_ = f.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Count(strings.TrimRight(string(raw), "\n"), "\n") != 0 {
		t.Fatalf("the handshake must be one line, got %q", raw)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode handshake %q: %v", raw, err)
	}
	if got["ready"] != true {
		t.Errorf("ready = %#v", got["ready"])
	}
	if got["port"] != float64(4242) {
		t.Errorf("port = %#v", got["port"])
	}
	if got["token"] != e.daemon.token {
		t.Errorf("token = %#v", got["token"])
	}
	if got["project"] != e.store.Root {
		t.Errorf("project = %#v, want %q", got["project"], e.store.Root)
	}
	if got["version"] != Version {
		t.Errorf("version = %#v, want %q", got["version"], Version)
	}
}

// The desktop reads the version off a running daemon rather than spawning the
// binary again just to ask it.
func TestLocalStatusReportsTheVersion(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(http.MethodGet, "/api/local/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := decode[map[string]any](t, rec)
	if got["version"] != Version {
		t.Errorf("version = %#v, want %q", got["version"], Version)
	}
	if got["commit"] != Commit {
		t.Errorf("commit = %#v, want %q", got["commit"], Commit)
	}
	// The fields the status bar already relied on are untouched.
	for _, field := range []string{"project", "workflows", "providers", "cli", "local_nodes"} {
		if _, ok := got[field]; !ok {
			t.Errorf("status lost its %q field", field)
		}
	}
}

// --node-types is what a build elsewhere asks to check a palette it maintains
// by hand. It answers on a binary with no project folder, like --version, and
// it lists exactly what this engine offers.
func TestNodeTypesFlagListsWhatTheEngineOffers(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "zyvrod-under-test")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--node-types").Output()
	if err != nil {
		t.Fatalf("--node-types exited non-zero: %v", err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		got = append(got, strings.TrimSpace(line))
	}
	want := engine.RunnableBuiltinNodeTypes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("--node-types printed %v, want %v", got, want)
	}

	// A disabled node is a name a pack may not take, and still not a node
	// anyone can place: it must not reach a palette. generateVideo used to be
	// the example here and is now the counter-example — it runs, so it belongs
	// in the palette — which leaves the rule stated against the list itself.
	disabled := map[string]bool{}
	for _, name := range engine.BuiltinNodeTypes() {
		disabled[name] = true
	}
	for _, name := range engine.RunnableBuiltinNodeTypes() {
		delete(disabled, name)
	}
	for _, name := range got {
		if disabled[name] {
			t.Errorf("%s is disabled and was offered as runnable", name)
		}
	}
	if !containsString(got, "generateVideo") {
		t.Error("generateVideo runs now: it belongs in the palette")
	}
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
