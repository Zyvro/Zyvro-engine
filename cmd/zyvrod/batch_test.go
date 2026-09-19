package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Zyvro/Zyvro-engine/localstore"
)

// copyGraph is the whole point of the feature in one graph: a file input whose
// path is a run input, and a file output named after what was read. It calls no
// model — the batch is about repetition, not about what is repeated, and a test
// that needed a provider key would test the provider.
const copyGraph = `{
  "nodes": [
    {"id":"in","type":"fileInput","position":{"x":0,"y":0},"data":{"label":"Source","config":{"path":"{{input:gfx}}","as":"text"}}},
    {"id":"out","type":"fileOutput","position":{"x":300,"y":0},"data":{"config":{"path":"done/{{sourceStem}}-hd{{sourceExt}}","createDirs":true}}}
  ],
  "edges": [
    {"id":"e1","source":"in","target":"out","sourceHandle":"out","targetHandle":"in","type":"data"}
  ]
}`

type batchEnvelope struct {
	Batch  localstore.Batch       `json:"batch"`
	Counts localstore.BatchCounts `json:"counts"`
}

type batchStart struct {
	BatchID string `json:"batch_id"`
	Total   int    `json:"total"`
	Status  string `json:"status"`
	Retried int    `json:"retried"`
}

func (e *testEnv) writeProjectFile(rel, body string) {
	e.t.Helper()
	abs := filepath.Join(e.store.Root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) readProjectFile(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.store.Root, filepath.FromSlash(rel)))
	if err != nil {
		e.t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func (e *testEnv) workflow(name, graph string) localstore.Workflow {
	e.t.Helper()
	return decode[localstore.Workflow](e.t, mustStatus(e.t,
		e.do(http.MethodPost, "/api/workflows", map[string]any{
			"name":       name,
			"graph_json": json.RawMessage(graph),
		}), http.StatusCreated))
}

// waitForBatch polls exactly as a caller would, until the batch settles.
func (e *testEnv) waitForBatch(id string) batchEnvelope {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var env batchEnvelope
	for time.Now().Before(deadline) {
		rec := e.do(http.MethodGet, "/api/batches/"+id, nil)
		if rec.Code != http.StatusOK {
			e.t.Fatalf("poll status = %d: %s", rec.Code, rec.Body.String())
		}
		env = decode[batchEnvelope](e.t, rec)
		switch env.Batch.Status {
		case "completed", "failed", "cancelled":
			return env
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("batch %s never finished (last status %q)", id, env.Batch.Status)
	return env
}

// The form the whole document was about: a folder in, one output each, named
// after the file it came from. Nothing is edited between the runs.
func TestBatchRunsOneExecutionPerFile(t *testing.T) {
	e := newTestEnv(t)
	wf := e.workflow("Upscale", copyGraph)
	e.writeProjectFile("sprites/53013.txt", "first")
	e.writeProjectFile("sprites/10021.txt", "second")
	e.writeProjectFile("sprites/99999.txt", "third")

	start := decode[batchStart](t, mustStatus(t, e.do(http.MethodPost, "/api/batches", map[string]any{
		"workflow_id": wf.ID,
		"input":       "gfx",
		"dir":         "sprites",
		"match":       "*.txt",
	}), http.StatusAccepted))
	if start.Total != 3 || !localstore.ValidID(start.BatchID) {
		t.Fatalf("start = %+v", start)
	}

	env := e.waitForBatch(start.BatchID)
	if env.Batch.Status != "completed" {
		t.Fatalf("status = %q (%s), items = %+v", env.Batch.Status, env.Batch.Error, env.Batch.Items)
	}
	if env.Counts.Completed != 3 || env.Counts.Failed != 0 {
		t.Fatalf("counts = %+v", env.Counts)
	}

	// Each file produced its own output, under its own name, holding its own
	// contents. One of these three being wrong is the failure the whole
	// provenance mechanism exists to prevent.
	for rel, want := range map[string]string{
		"done/53013-hd.txt": "first",
		"done/10021-hd.txt": "second",
		"done/99999-hd.txt": "third",
	} {
		if got := e.readProjectFile(rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}

	// Three real executions, each on the disk, each carrying the batch id. That
	// field is what lets one line stand for them without a second history.
	execs := decode[[]localstore.Execution](t, mustStatus(t,
		e.do(http.MethodGet, "/api/workflows/"+wf.ID+"/executions", nil), http.StatusOK))
	if len(execs) != 3 {
		t.Fatalf("executions = %d, want 3", len(execs))
	}
	for _, x := range execs {
		if x.BatchID != start.BatchID {
			t.Errorf("execution %s batch_id = %q", x.ID, x.BatchID)
		}
		if x.Status != "completed" {
			t.Errorf("execution %s status = %q (%s)", x.ID, x.Status, x.Error)
		}
	}
	// And every item points at a run someone can open.
	for _, it := range env.Batch.Items {
		if !localstore.ValidID(it.ExecutionID) {
			t.Errorf("item %s has no execution: %+v", it.Path, it)
		}
	}

	// The history row: one line, with the count on it.
	rows := decode[[]batchRow](t, mustStatus(t,
		e.do(http.MethodGet, "/api/workflows/"+wf.ID+"/batches", nil), http.StatusOK))
	if len(rows) != 1 || rows[0].Counts.Completed != 3 || rows[0].Status != "completed" {
		t.Fatalf("rows = %+v", rows)
	}
}

// An explicit list of files rather than a folder: the second of the three
// forms, and the one an agent or a script drives.
func TestBatchAcceptsAnExplicitList(t *testing.T) {
	e := newTestEnv(t)
	wf := e.workflow("Chosen", copyGraph)
	e.writeProjectFile("a.txt", "A")
	e.writeProjectFile("deep/b.txt", "B")
	e.writeProjectFile("ignored.txt", "C")

	start := decode[batchStart](t, mustStatus(t, e.do(http.MethodPost, "/api/batches", map[string]any{
		"workflow_id": wf.ID,
		"input":       "gfx",
		"paths":       []string{"a.txt", "deep/b.txt"},
	}), http.StatusAccepted))
	env := e.waitForBatch(start.BatchID)
	if env.Batch.Status != "completed" || env.Counts.Total != 2 {
		t.Fatalf("batch = %+v", env)
	}
	if e.readProjectFile("done/a-hd.txt") != "A" || e.readProjectFile("done/b-hd.txt") != "B" {
		t.Error("the two chosen files did not produce their outputs")
	}
	if _, err := os.Stat(filepath.Join(e.store.Root, "done", "ignored-hd.txt")); err == nil {
		t.Error("a file that was not in the list was run anyway")
	}
}

// The expensive mistake, refused before the first run: a batch drives one input
// 519 times, and an input that reaches nothing means 519 runs reading the same
// file, all reporting success.
func TestBatchRefusesAnInputTheGraphNeverReads(t *testing.T) {
	e := newTestEnv(t)
	wf := e.workflow("Upscale", copyGraph)
	e.writeProjectFile("sprites/one.txt", "x")

	rec := e.do(http.MethodPost, "/api/batches", map[string]any{
		"workflow_id": wf.ID,
		"input":       "sprite",
		"dir":         "sprites",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sprite") {
		t.Errorf("the refusal does not name the input: %s", rec.Body.String())
	}
	// And the one the graph does read is accepted. Waited out rather than left
	// running: a batch outlives the request that started it, which is the
	// whole point of it, and a test that walked away here would be deleting
	// the project folder under a live driver.
	ok := decode[batchStart](t, mustStatus(t, e.do(http.MethodPost, "/api/batches", map[string]any{
		"workflow_id": wf.ID, "input": "gfx", "dir": "sprites",
	}), http.StatusAccepted))
	if env := e.waitForBatch(ok.BatchID); env.Batch.Status != "completed" {
		t.Fatalf("accepted batch = %q", env.Batch.Status)
	}
}

// Nothing runs until the selection makes sense: an empty match, a folder that
// does not exist, both inputs at once, a count nobody asked for.
func TestBatchRefusalsHappenBeforeAnythingRuns(t *testing.T) {
	e := newTestEnv(t)
	wf := e.workflow("Upscale", copyGraph)
	e.writeProjectFile("sprites/a.txt", "1")
	e.writeProjectFile("sprites/b.txt", "2")
	e.writeProjectFile("sprites/c.txt", "3")

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no input named", map[string]any{"workflow_id": wf.ID, "dir": "sprites"}, "input required"},
		{"unknown workflow", map[string]any{"workflow_id": localstore.NewID(), "input": "gfx"}, "workflow not found"},
		{"nothing matches", map[string]any{"workflow_id": wf.ID, "input": "gfx", "dir": "sprites", "match": "*.png"}, "matches"},
		{"missing folder", map[string]any{"workflow_id": wf.ID, "input": "gfx", "dir": "nowhere"}, "no such folder"},
		{"outside the project", map[string]any{"workflow_id": wf.ID, "input": "gfx", "dir": "../.."}, "project folder"},
		{"both ways at once", map[string]any{"workflow_id": wf.ID, "input": "gfx", "dir": "sprites", "paths": []string{"sprites/a.txt"}}, "not both"},
		{"too many files", map[string]any{"workflow_id": wf.ID, "input": "gfx", "dir": "sprites", "max_items": 2}, "max_items"},
		{"bad pattern", map[string]any{"workflow_id": wf.ID, "input": "gfx", "dir": "sprites", "match": "[bad"}, "not a valid pattern"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(http.MethodPost, "/api/batches", tc.body)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("status = %d, want a refusal: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body = %s, want it to mention %q", rec.Body.String(), tc.want)
			}
		})
	}

	// Nothing was written, and nothing ran.
	if batches, _ := e.store.ListBatches("", 0); len(batches) != 0 {
		t.Errorf("refused batches were stored: %d", len(batches))
	}
	if runs, _ := e.store.ListRuns("", 0); len(runs) != 0 {
		t.Errorf("refused batches started runs: %d", len(runs))
	}
}

// The third arbitrage. A batch that failed part way through resumes: what is
// already written — and already paid for — is not done again.
func TestBatchRetryResumesAndRestartRedoes(t *testing.T) {
	e := newTestEnv(t)
	wf := e.workflow("Upscale", copyGraph)
	e.writeProjectFile("sprites/ok1.txt", "one")
	e.writeProjectFile("sprites/ok2.txt", "two")

	// A third path that is not there yet: its item fails, the other two do not.
	start := decode[batchStart](t, mustStatus(t, e.do(http.MethodPost, "/api/batches", map[string]any{
		"workflow_id": wf.ID,
		"input":       "gfx",
		"paths":       []string{"sprites/ok1.txt", "sprites/later.txt", "sprites/ok2.txt"},
	}), http.StatusAccepted))

	env := e.waitForBatch(start.BatchID)
	if env.Batch.Status != "failed" || env.Counts.Completed != 2 || env.Counts.Failed != 1 {
		t.Fatalf("first attempt = %q %+v", env.Batch.Status, env.Counts)
	}
	// A batch where one of three failed says so rather than reporting success.
	if !strings.Contains(env.Batch.Error, "1 of 3") {
		t.Errorf("batch error = %q", env.Batch.Error)
	}
	done := map[string]string{}
	for _, it := range env.Batch.Items {
		if it.Status == "completed" {
			done[it.Path] = it.ExecutionID
		}
	}
	if len(done) != 2 {
		t.Fatalf("completed items = %+v", env.Batch.Items)
	}

	// Now the missing file appears, and the batch is resumed.
	e.writeProjectFile("sprites/later.txt", "three")
	again := decode[batchStart](t, mustStatus(t,
		e.do(http.MethodPost, "/api/batches/"+start.BatchID+"/retry", nil), http.StatusAccepted))
	if again.Retried != 1 {
		t.Fatalf("retried = %d, want only the one that failed", again.Retried)
	}
	env = e.waitForBatch(start.BatchID)
	if env.Batch.Status != "completed" || env.Counts.Completed != 3 {
		t.Fatalf("after retry = %q %+v", env.Batch.Status, env.Counts)
	}
	if e.readProjectFile("done/later-hd.txt") != "three" {
		t.Error("the resumed item did not write its output")
	}
	// The two that had completed kept the run they completed with: they were
	// not run again.
	for _, it := range env.Batch.Items {
		if was, ok := done[it.Path]; ok && was != it.ExecutionID {
			t.Errorf("item %s was run again on retry (%s -> %s)", it.Path, was, it.ExecutionID)
		}
	}
	if runs, _ := e.store.ListRuns(wf.ID, 0); len(runs) != 4 {
		t.Errorf("runs = %d, want 4: three items plus the one retry", len(runs))
	}

	// Restart is the other word, and it means all of them.
	all := decode[batchStart](t, mustStatus(t,
		e.do(http.MethodPost, "/api/batches/"+start.BatchID+"/retry", map[string]any{"restart": true}), http.StatusAccepted))
	if all.Retried != 3 {
		t.Fatalf("restart retried = %d, want 3", all.Retried)
	}
	env = e.waitForBatch(start.BatchID)
	if env.Batch.Status != "completed" {
		t.Fatalf("after restart = %q", env.Batch.Status)
	}
	if runs, _ := e.store.ListRuns(wf.ID, 0); len(runs) != 7 {
		t.Errorf("runs = %d, want 7: a restart runs all three again", len(runs))
	}

	// A resume with nothing left to do says so rather than running anything.
	idle := decode[batchStart](t, mustStatus(t,
		e.do(http.MethodPost, "/api/batches/"+start.BatchID+"/retry", nil), http.StatusOK))
	if idle.Retried != 0 {
		t.Errorf("idle retry = %+v", idle)
	}
}

// Stop means stop. A batch is the one thing here that runs for hours, so the
// items that have not started must not start — and the count has to show it.
func TestBatchCancelStopsTheRest(t *testing.T) {
	e := newTestEnv(t)
	wf := e.workflow("Upscale", copyGraph)
	const total = 120
	paths := make([]string, 0, total)
	for i := 0; i < total; i++ {
		rel := fmt.Sprintf("sprites/%03d.txt", i)
		e.writeProjectFile(rel, fmt.Sprint(i))
		paths = append(paths, rel)
	}

	start := decode[batchStart](t, mustStatus(t, e.do(http.MethodPost, "/api/batches", map[string]any{
		"workflow_id": wf.ID, "input": "gfx", "paths": paths,
	}), http.StatusAccepted))

	// Cancel once the batch is demonstrably under way, so this is a batch being
	// stopped rather than one that never started. There are 119 items left at
	// that point, which is a wide enough margin that the cancel cannot be
	// racing the end.
	deadline := time.Now().Add(30 * time.Second)
	for {
		env := decode[batchEnvelope](t, mustStatus(t, e.do(http.MethodGet, "/api/batches/"+start.BatchID, nil), http.StatusOK))
		if env.Counts.Completed >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch never got going: %+v", env.Counts)
		}
		time.Sleep(time.Millisecond)
	}
	mustStatus(t, e.do(http.MethodPost, "/api/batches/"+start.BatchID+"/cancel", nil), http.StatusOK)

	env := e.waitForBatch(start.BatchID)
	if env.Batch.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", env.Batch.Status)
	}
	if env.Counts.Completed >= total {
		t.Fatalf("every item ran anyway: %+v", env.Counts)
	}
	if env.Counts.Pending != 0 || env.Counts.Running != 0 {
		t.Errorf("items left in limbo: %+v", env.Counts)
	}
	// And what it did get through is still there: cancelling does not undo.
	if env.Counts.Completed > 0 && e.readProjectFile("done/000-hd.txt") != "0" {
		t.Error("the work done before the cancel was lost")
	}
	// Resuming picks up where it stopped.
	again := decode[batchStart](t, mustStatus(t,
		e.do(http.MethodPost, "/api/batches/"+start.BatchID+"/retry", nil), http.StatusAccepted))
	if again.Retried != total-env.Counts.Completed {
		t.Errorf("retried = %d, want the %d that did not complete", again.Retried, total-env.Counts.Completed)
	}
	final := e.waitForBatch(start.BatchID)
	if final.Batch.Status != "completed" || final.Counts.Completed != total {
		t.Fatalf("after resume = %q %+v", final.Batch.Status, final.Counts)
	}
}

// A batch left "running" by a daemon that went away is the one case worth
// writing: nothing else will ever finish it, and it would claim to be working
// for good.
func TestCancelSettlesABatchNothingIsDriving(t *testing.T) {
	e := newTestEnv(t)
	b := &localstore.Batch{
		ID: localstore.NewID(), WorkflowID: localstore.NewID(), Input: "gfx",
		Status: "running", CreatedAt: time.Now().UTC(),
		Items: []localstore.BatchItem{
			{Path: "a.txt", Status: "completed", ExecutionID: localstore.NewID()},
			{Path: "b.txt", Status: "running"},
			{Path: "c.txt", Status: "pending"},
		},
	}
	if err := e.store.SaveBatch(b); err != nil {
		t.Fatal(err)
	}
	rec := mustStatus(t, e.do(http.MethodPost, "/api/batches/"+b.ID+"/cancel", nil), http.StatusOK)
	if strings.Contains(rec.Body.String(), `"stopping":true`) {
		t.Errorf("claimed to be stopping a batch nothing is driving: %s", rec.Body.String())
	}
	env := decode[batchEnvelope](t, mustStatus(t, e.do(http.MethodGet, "/api/batches/"+b.ID, nil), http.StatusOK))
	if env.Batch.Status != "cancelled" || env.Counts.Completed != 1 || env.Counts.Cancelled != 2 {
		t.Fatalf("batch = %q %+v", env.Batch.Status, env.Counts)
	}
}

// The listing a batch is built from goes through the same gate the file nodes
// do, and a glob is matched on the name rather than the whole path — "*.txt"
// is what a person means by "the text files".
func TestBatchDiscoveryMatchesOnTheName(t *testing.T) {
	e := newTestEnv(t)
	wf := e.workflow("Upscale", copyGraph)
	e.writeProjectFile("sprites/a.txt", "1")
	e.writeProjectFile("sprites/b.png", "2")
	e.writeProjectFile("sprites/deep/c.txt", "3")

	start := decode[batchStart](t, mustStatus(t, e.do(http.MethodPost, "/api/batches", map[string]any{
		"workflow_id": wf.ID, "input": "gfx", "dir": "sprites", "match": "*.txt", "recursive": true,
	}), http.StatusAccepted))
	env := e.waitForBatch(start.BatchID)

	got := make([]string, 0, len(env.Batch.Items))
	for _, it := range env.Batch.Items {
		got = append(got, it.Path)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "sprites/a.txt,sprites/deep/c.txt" {
		t.Errorf("items = %v", got)
	}
}
