// Un graphe lancé sur un dossier.
//
// `workflows-and-nodes-todo.md` demandait trois formes : un fichier vers un
// fichier, N vers N, un dossier vers N. La première est le travail du moteur et
// il le fait depuis la provenance. Les deux autres sont le même graphe relancé,
// et ce qui change n'est pas le traitement mais **ce qui pilote la répétition**.
// Ce fichier est ce pilote, et il est au-dessus du graphe : le DAG ne sait
// toujours pas boucler, ce qui reste vrai le jour où il l'apprendra pour le
// seul cas qui le réclame vraiment, l'agrégation.
//
// Les trois arbitrages que le document laissait ouverts, tranchés :
//
//   - **Un appel d'API, pas un nœud.** Un lot n'est pas dans le graphe, il le
//     lance ; un nœud dont l'exécution serait d'autres exécutions serait un
//     nœud qui ment sur ce qu'est un nœud. Le bouton de Studio appelle ceci.
//   - **519 exécutions, une ligne.** Chaque élément doit être une vraie
//     exécution — isolée, cachable, annulable, sur le disque — et un échec ne
//     doit pas emporter les 518 autres. Ce que personne ne veut, c'est 519
//     lignes dans l'historique. Donc les exécutions restent des exécutions et
//     portent l'`id` du lot qui les a lancées : une seule liste, une clef de
//     regroupement, pas un second historique à tenir en phase avec le premier.
//   - **Un lot qui casse au 300ᵉ reprend.** 299 fichiers sont écrits, plusieurs
//     payés à un modèle. Recommencer est un mot distinct, qu'il faut dire.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Zyvro/Zyvro-engine/engine"
	"github.com/Zyvro/Zyvro-engine/localstore"
)

// defaultMaxBatchItems is how many files a batch runs over without being told
// to. It is not a technical limit — nothing breaks at 1001 — it is the
// difference between "run this over my sprites" and "run this over
// node_modules", and the second one costs money before anyone notices. The
// request can raise it; the point is that raising it is a decision.
const defaultMaxBatchItems = 1000

type batchRequest struct {
	WorkflowID string `json:"workflow_id"`
	// Input is the run input each file's path is passed as: a graph whose
	// fileInput path reads {{input:gfx}} is driven with "gfx".
	Input string `json:"input"`
	// Dir, Match and Recursive discover the files (form 3). Paths gives them
	// outright (form 2). One or the other.
	Dir       string   `json:"dir"`
	Match     string   `json:"match"`
	Recursive bool     `json:"recursive"`
	Paths     []string `json:"paths"`
	// Inputs is what every item is run with besides its own path.
	Inputs   map[string]any `json:"inputs"`
	UseCache *bool          `json:"use_cache"`
	MaxItems int            `json:"max_items"`
}

// createBatch is POST /api/batches. Like POST /api/executions it answers as
// soon as the work is accepted, because the work is hours long and the caller
// polls.
func (d *daemon) createBatch(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if !readJSON(w, r, &req) {
		return
	}
	b, err := d.newBatch(req)
	if err != nil {
		writeRunErr(w, err, "failed to create batch")
		return
	}
	d.runs.Add(1)
	go d.driveBatch(b)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"batch_id": b.ID,
		"total":    len(b.Items),
		"status":   "queued",
	})
}

// newBatch decides what the batch will run over and writes the plan, before a
// single run starts. Everything that can be refused is refused here: the point
// of a batch is that you walk away from it, so the questions have to be asked
// while someone is still watching.
func (d *daemon) newBatch(req batchRequest) (*localstore.Batch, error) {
	input := strings.TrimSpace(req.Input)
	if input == "" {
		return nil, &runError{http.StatusBadRequest, "input required: name the run input each file's path is passed as"}
	}
	wf, graph, err := d.runnableWorkflow(req.WorkflowID)
	if err != nil {
		return nil, err
	}
	// The expensive mistake, caught before the first run: an input that reaches
	// nothing means 519 runs that all read the same file and write over the
	// same output, every one of them reporting success.
	if !engine.GraphUsesInput(graph, input) {
		return nil, &runError{http.StatusBadRequest, fmt.Sprintf(
			"nothing in this workflow reads an input named %q: point a file input's path at {{input:%s}}, or give an input node that key",
			input, input)}
	}
	paths, err := d.batchPaths(req)
	if err != nil {
		return nil, err
	}

	inputsJSON := "{}"
	if req.Inputs != nil {
		b, _ := json.Marshal(req.Inputs)
		inputsJSON = string(b)
	}
	items := make([]localstore.BatchItem, 0, len(paths))
	for _, p := range paths {
		items = append(items, localstore.BatchItem{Path: p, Status: "pending"})
	}
	batch := &localstore.Batch{
		ID:              localstore.NewID(),
		WorkflowID:      wf.ID,
		UserID:          localstore.LocalUserID,
		WorkflowVersion: wf.Version,
		Input:           input,
		Dir:             strings.TrimSpace(req.Dir),
		Match:           strings.TrimSpace(req.Match),
		Recursive:       req.Recursive,
		Status:          "queued",
		InputJSON:       inputsJSON,
		UseCache:        req.UseCache == nil || *req.UseCache,
		Items:           items,
		CreatedAt:       time.Now().UTC(),
	}
	if err := d.store.SaveBatch(batch); err != nil {
		return nil, &runError{http.StatusInternalServerError, "failed to create batch"}
	}
	return batch, nil
}

// batchPaths is the list of files, given or discovered.
func (d *daemon) batchPaths(req batchRequest) ([]string, error) {
	max := req.MaxItems
	if max <= 0 {
		max = defaultMaxBatchItems
	}
	pattern := strings.TrimSpace(req.Match)
	if pattern != "" {
		if _, err := path.Match(pattern, "x"); err != nil {
			return nil, &runError{http.StatusBadRequest, fmt.Sprintf("match %q is not a valid pattern", pattern)}
		}
	}

	var paths []string
	switch {
	case len(req.Paths) > 0:
		if strings.TrimSpace(req.Dir) != "" {
			return nil, &runError{http.StatusBadRequest, "give paths or dir, not both: one says which files, the other where to find them"}
		}
		for _, p := range req.Paths {
			if t := strings.TrimSpace(p); t != "" {
				paths = append(paths, t)
			}
		}
		paths = matching(paths, pattern)
	default:
		// The listing goes through the interface rather than the concrete
		// store, so an engine built without a project folder — the hosted one —
		// refuses here instead of pretending to find nothing.
		lister, ok := any(d.store.Files()).(engine.DirLister)
		if !ok {
			return nil, &runError{http.StatusInternalServerError, "this engine cannot list folders"}
		}
		found, err := lister.List(req.Dir, req.Recursive)
		if err != nil {
			return nil, &runError{http.StatusBadRequest, err.Error()}
		}
		paths = matching(found, pattern)
	}

	if len(paths) == 0 {
		return nil, &runError{http.StatusBadRequest, "no files to run over: " + describeSelection(req)}
	}
	if len(paths) > max {
		return nil, &runError{http.StatusBadRequest, fmt.Sprintf(
			"%d files matched, over the %d a batch runs over without being told to: narrow it with match, or raise max_items",
			len(paths), max)}
	}
	return paths, nil
}

// matching keeps the paths a glob selects. A pattern containing a slash is
// matched against the whole path, one without against the file name alone —
// "*.png" is what a person means by "the pngs", not "the pngs at the top
// level".
func matching(paths []string, pattern string) []string {
	if pattern == "" {
		return paths
	}
	whole := strings.Contains(pattern, "/")
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		subject := p
		if !whole {
			subject = path.Base(p)
		}
		if ok, err := path.Match(pattern, subject); err == nil && ok {
			out = append(out, p)
		}
	}
	return out
}

// describeSelection says what was looked at, because "no files" is only useful
// next to where we looked.
func describeSelection(req batchRequest) string {
	dir := strings.TrimSpace(req.Dir)
	if len(req.Paths) > 0 {
		dir = "the paths given"
	} else if dir == "" {
		dir = "the project folder"
	}
	if m := strings.TrimSpace(req.Match); m != "" {
		return fmt.Sprintf("nothing under %s matches %s", dir, m)
	}
	return fmt.Sprintf("%s holds no files", dir)
}

// ---------- the driver ----------

// driveBatch runs the items one after another.
//
// One at a time, deliberately. Each item is a real run competing for the same
// providers, the same rate limits and the same project folder, and a batch that
// saturates the machine is a batch you cannot use the app during. 519 sprites
// are something you start and come back to.
func (d *daemon) driveBatch(b *localstore.Batch) {
	defer d.runs.Done()

	ctx, cancel := context.WithCancel(context.Background())
	d.batchMu.Lock()
	if d.batchStop == nil {
		d.batchStop = map[string]context.CancelFunc{}
	}
	d.batchStop[b.ID] = cancel
	d.batchMu.Unlock()
	defer func() {
		d.batchMu.Lock()
		delete(d.batchStop, b.ID)
		d.batchMu.Unlock()
		cancel()
	}()

	rec := &batchRecorder{store: d.store, batch: b}
	rec.update(func(x *localstore.Batch) {
		x.Status = "running"
		x.Error = ""
		x.FinishedAt = nil
		if x.StartedAt == nil {
			x.StartedAt = ptrTime(time.Now().UTC())
		}
	})

	shared := map[string]any{}
	if s := strings.TrimSpace(b.InputJSON); s != "" {
		_ = json.Unmarshal([]byte(s), &shared)
	}

	for i := 0; i < rec.count(); i++ {
		// Resume: what completed is not done again. This is the third
		// arbitrage, and it is one line because the plan on disk is what makes
		// it one line.
		if rec.item(i).Status == "completed" {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		d.runBatchItem(ctx, rec, i, shared)
	}
	rec.finish(ctx.Err() != nil)
}

// runBatchItem is one file: one real run, prepared and executed here rather
// than handed to a goroutine, because the next item must not start until this
// one is done.
func (d *daemon) runBatchItem(ctx context.Context, rec *batchRecorder, i int, shared map[string]any) {
	item := rec.item(i)
	useCache := rec.useCache()

	inputs := make(map[string]any, len(shared)+1)
	for k, v := range shared {
		inputs[k] = v
	}
	inputs[rec.inputName()] = item.Path

	run, wf, graph, err := d.prepareRun(runRequest{
		WorkflowID: rec.workflowID(),
		Inputs:     inputs,
		UseCache:   &useCache,
		batchID:    rec.id(),
	})
	if err != nil {
		// A refusal at this point is about the workflow, not the file, so it
		// would refuse every remaining item too — but the batch carries on, and
		// each item says the same thing, rather than the batch stopping on a
		// judgement it made about files it has not looked at.
		rec.setItem(i, "failed", "", err.Error())
		return
	}
	rec.setItem(i, "running", run.ID, "")

	runCtx, cancelRun := context.WithTimeout(ctx, executionTimeout)
	d.execute(runCtx, run, wf, graph, inputs, useCache, "")
	cancelRun()

	// The run file is the record of what the run did; the item only has to know
	// whether it still needs doing. Read back rather than assumed: execute
	// writes the outcome, including a cancellation, and inventing it here would
	// be a second account of the same event.
	status, msg := "failed", "the run left no record"
	if fin, readErr := d.store.GetRun(run.ID); readErr == nil {
		status, msg = fin.Status, fin.Error
	}
	rec.setItem(i, status, run.ID, msg)
}

// batchRecorder owns the batch file while the batch runs. Same shape as
// runRecorder and for the same reason: one writer, the whole record rewritten
// atomically on every transition, so a poll never has to stitch anything
// together.
type batchRecorder struct {
	store *localstore.Store

	mu    sync.Mutex
	batch *localstore.Batch
}

func (br *batchRecorder) update(fn func(*localstore.Batch)) {
	br.mu.Lock()
	defer br.mu.Unlock()
	fn(br.batch)
	if err := br.store.SaveBatch(br.batch); err != nil {
		log.Printf("batch %s: persist failed: %v", br.batch.ID, err)
	}
}

func (br *batchRecorder) read(fn func(*localstore.Batch)) {
	br.mu.Lock()
	defer br.mu.Unlock()
	fn(br.batch)
}

func (br *batchRecorder) count() int {
	n := 0
	br.read(func(b *localstore.Batch) { n = len(b.Items) })
	return n
}

func (br *batchRecorder) item(i int) localstore.BatchItem {
	var it localstore.BatchItem
	br.read(func(b *localstore.Batch) {
		if i >= 0 && i < len(b.Items) {
			it = b.Items[i]
		}
	})
	return it
}

func (br *batchRecorder) id() string {
	s := ""
	br.read(func(b *localstore.Batch) { s = b.ID })
	return s
}
func (br *batchRecorder) workflowID() string {
	s := ""
	br.read(func(b *localstore.Batch) { s = b.WorkflowID })
	return s
}
func (br *batchRecorder) inputName() string {
	s := ""
	br.read(func(b *localstore.Batch) { s = b.Input })
	return s
}
func (br *batchRecorder) useCache() bool {
	v := false
	br.read(func(b *localstore.Batch) { v = b.UseCache })
	return v
}

func (br *batchRecorder) setItem(i int, status, executionID, msg string) {
	br.update(func(b *localstore.Batch) {
		if i < 0 || i >= len(b.Items) {
			return
		}
		b.Items[i].Status = status
		if executionID != "" {
			b.Items[i].ExecutionID = executionID
		}
		b.Items[i].Error = msg
	})
}

// finish settles the batch's own status. "completed" means every item
// completed: a batch where three of 519 failed is failed and says which three,
// rather than reporting success and leaving someone to find the three.
func (br *batchRecorder) finish(cancelled bool) {
	br.update(func(b *localstore.Batch) {
		if cancelled {
			for i := range b.Items {
				if b.Items[i].Status == "pending" || b.Items[i].Status == "running" {
					b.Items[i].Status = "cancelled"
				}
			}
			b.Status = "cancelled"
		} else if c := b.Counts(); c.Failed > 0 {
			b.Status = "failed"
			b.Error = fmt.Sprintf("%d of %d failed", c.Failed, c.Total)
		} else {
			b.Status = "completed"
		}
		b.FinishedAt = ptrTime(time.Now().UTC())
	})
}

// ---------- reading and steering ----------

func (d *daemon) getBatch(w http.ResponseWriter, r *http.Request) {
	b, err := d.store.GetBatch(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err, "batch not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": b, "counts": b.Counts()})
}

// batchRow is a batch without its items: what a history list shows. The items
// of twenty batches would be ten thousand entries in a list nobody scrolls that
// far into, and the batch itself is one request away.
type batchRow struct {
	ID         string                 `json:"id"`
	WorkflowID string                 `json:"workflow_id"`
	Input      string                 `json:"input"`
	Dir        string                 `json:"dir,omitempty"`
	Match      string                 `json:"match,omitempty"`
	Status     string                 `json:"status"`
	Error      string                 `json:"error,omitempty"`
	Counts     localstore.BatchCounts `json:"counts"`
	CreatedAt  time.Time              `json:"created_at"`
	StartedAt  *time.Time             `json:"started_at"`
	FinishedAt *time.Time             `json:"finished_at"`
}

func (d *daemon) listWorkflowBatches(w http.ResponseWriter, r *http.Request) {
	batches, err := d.store.ListBatches(r.PathValue("id"), 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list batches")
		return
	}
	out := make([]batchRow, 0, len(batches))
	for i := range batches {
		b := &batches[i]
		out = append(out, batchRow{
			ID: b.ID, WorkflowID: b.WorkflowID, Input: b.Input, Dir: b.Dir, Match: b.Match,
			Status: b.Status, Error: b.Error, Counts: b.Counts(),
			CreatedAt: b.CreatedAt, StartedAt: b.StartedAt, FinishedAt: b.FinishedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// retryBatch is POST /api/batches/{id}/retry. It resumes: items that completed
// are left alone. `{"restart": true}` redoes everything, which is a different
// thing to want and so a different thing to say.
func (d *daemon) retryBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Restart bool `json:"restart"`
	}
	// An empty body is the common case — "carry on" — so a missing or unparsable
	// body means resume rather than a 400.
	_ = json.NewDecoder(r.Body).Decode(&body)

	b, err := d.store.GetBatch(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err, "batch not found")
		return
	}
	if d.batchIsRunning(b.ID) {
		writeErr(w, http.StatusConflict, "this batch is still running; cancel it first")
		return
	}

	redo := 0
	for i := range b.Items {
		if !body.Restart && b.Items[i].Status == "completed" {
			continue
		}
		// The execution id is cleared with the status: leaving it would point
		// at the failed run while the item claims to be waiting. That run is
		// still in the history, under this batch's id.
		b.Items[i] = localstore.BatchItem{Path: b.Items[i].Path, Status: "pending"}
		redo++
	}
	if redo == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"batch_id": b.ID, "retried": 0, "status": b.Status})
		return
	}
	b.Status = "queued"
	b.Error = ""
	b.FinishedAt = nil
	if err := d.store.SaveBatch(b); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update batch")
		return
	}
	d.runs.Add(1)
	go d.driveBatch(b)
	writeJSON(w, http.StatusAccepted, map[string]any{"batch_id": b.ID, "retried": redo, "status": "queued"})
}

// cancelBatch stops a batch, including the run in flight: the item contexts
// hang off the batch's, so "stop" means stop, not "stop after this one".
func (d *daemon) cancelBatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := d.store.GetBatch(id)
	if err != nil {
		writeStoreErr(w, err, "batch not found")
		return
	}
	d.batchMu.Lock()
	cancel, live := d.batchStop[id]
	d.batchMu.Unlock()
	if live {
		cancel()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopping": true})
		return
	}
	// Not running here. A batch left "running" by a daemon that went away is
	// the case worth writing: nothing else will ever finish it, and it would
	// claim to be working for good.
	if b.Status == "queued" || b.Status == "running" {
		for i := range b.Items {
			if b.Items[i].Status == "pending" || b.Items[i].Status == "running" {
				b.Items[i].Status = "cancelled"
			}
		}
		b.Status = "cancelled"
		b.FinishedAt = ptrTime(time.Now().UTC())
		_ = d.store.SaveBatch(b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopping": false})
}

func (d *daemon) batchIsRunning(id string) bool {
	d.batchMu.Lock()
	defer d.batchMu.Unlock()
	_, ok := d.batchStop[id]
	return ok
}

// writeRunErr reports a refusal that already knows its own status code, and
// falls back for anything else.
func writeRunErr(w http.ResponseWriter, err error, fallback string) {
	var re *runError
	if errors.As(err, &re) {
		writeErr(w, re.status, re.msg)
		return
	}
	writeErr(w, http.StatusInternalServerError, fallback)
}
