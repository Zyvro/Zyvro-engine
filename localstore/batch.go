package localstore

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// A batch is one workflow run over many files.
//
// `workflows-and-nodes-todo.md` asked for three forms: one file to one file, N
// files to N files, and a folder to N files. The first is the engine's job and
// it does it. The other two are the same graph run again and again, and what
// changes is only what drives the repetition — so the repetition lives here,
// above the graph, and the graph never learns to loop.
//
// Three things were left to decide when we got here. They are decided:
//
//  1. **A batch is an API call, not a node.** It is not in the graph — it
//     starts the graph — so making it a node would mean a node whose execution
//     is other executions. A button in Studio is how a person reaches it; the
//     call is the thing.
//
//  2. **519 sprites are 519 executions and one line.** Each item has to be a
//     real run: isolated, cached, cancellable, on the disk where the user can
//     read it, and one that fails must not take the other 518 with it. What
//     nobody wants is 519 lines in the history. So the runs stay runs, and each
//     one carries the id of the batch that started it — the grouping key. There
//     is one history, not two: this file is the *plan*, and the runs are the
//     record.
//
//  3. **A batch that fails at the 300th resumes.** It is the only answer that
//     respects what it already did: 299 files are written, several of them paid
//     for by a model call. Retry skips what completed; restart is a separate,
//     explicit word.
type Batch struct {
	ID              string `json:"id"`
	WorkflowID      string `json:"workflow_id"`
	UserID          string `json:"user_id"`
	WorkflowVersion int    `json:"workflow_version"`
	// Input is the run input each item's path is passed as: a graph whose
	// fileInput path is {{input:gfx}} is driven with Input "gfx".
	Input string `json:"input"`
	// Dir, Match and Recursive are how the item list was found, kept so a retry
	// does not have to be told again and so a person reading the file knows
	// what this batch was over. They are empty for a batch given an explicit
	// list of paths.
	Dir       string `json:"dir,omitempty"`
	Match     string `json:"match,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	// Status is the batch's own: queued, running, completed, failed or
	// cancelled. "completed" means every item completed — a batch where three
	// of 519 failed is failed, and says so, rather than reporting success and
	// hiding the three.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// InputJSON is what every item is run with besides its own path, and
	// UseCache whether unchanged nodes may be replayed. Both are kept because a
	// retry has to run the remaining items exactly as the first attempt would
	// have: a batch resumed under different inputs would write files that do
	// not match the ones already on disk, and nothing would say so.
	InputJSON  string      `json:"input_json"`
	UseCache   bool        `json:"use_cache"`
	Items      []BatchItem `json:"items"`
	CreatedAt  time.Time   `json:"created_at"`
	StartedAt  *time.Time  `json:"started_at"`
	FinishedAt *time.Time  `json:"finished_at"`
}

// BatchItem is one file and the run that was started for it.
//
// Status is the driver's own answer to "does this still need doing", written
// when it starts and finishes the item. It is not a second copy of the run's
// status pretending to be the truth: an item can fail before any run exists
// (a path the project refuses), and an item that has never run has no run file
// to ask. The run remains the record of what the run did.
type BatchItem struct {
	Path        string `json:"path"`
	ExecutionID string `json:"execution_id,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

// BatchCounts is the one-line summary: what a person wants from a row that
// stands for 519 runs. Derived on read from Items rather than stored, so it
// cannot drift from them.
type BatchCounts struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// Counts summarises the items.
func (b *Batch) Counts() BatchCounts {
	c := BatchCounts{Total: len(b.Items)}
	for _, it := range b.Items {
		switch it.Status {
		case "completed":
			c.Completed++
		case "failed":
			c.Failed++
		case "running":
			c.Running++
		case "cancelled":
			c.Cancelled++
		default:
			c.Pending++
		}
	}
	return c
}

// batchesDir sits inside runs/ rather than beside it. A batch is run history:
// it belongs with the runs, the generated .gitignore already excludes runs/ —
// so a project opened before batches existed does not end up committing them —
// and ListRuns skips directories, so it is invisible to the run listing that
// walks the same folder.
func (s *Store) batchesDir() string { return filepath.Join(s.runsDir(), "batches") }

func (s *Store) batchPath(id string) string {
	return filepath.Join(s.batchesDir(), id+".json")
}

// SaveBatch writes the whole batch record. Called on every item transition, so
// the write is atomic for the same reason SaveRun's is: a poll landing
// mid-write must never read a truncated file.
func (s *Store) SaveBatch(b *Batch) error {
	if b == nil {
		return errors.New("nil batch")
	}
	if !ValidID(b.ID) {
		return ErrInvalidID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.Items == nil {
		b.Items = []BatchItem{}
	}
	if err := os.MkdirAll(s.batchesDir(), 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(s.batchPath(b.ID), *b)
}

// GetBatch returns one batch.
func (s *Store) GetBatch(id string) (*Batch, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !ValidID(id) {
		return nil, ErrNotFound
	}
	var b Batch
	if err := readJSONFile(s.batchPath(id), &b); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if b.Items == nil {
		b.Items = []BatchItem{}
	}
	return &b, nil
}

// ListBatches returns a workflow's batches, newest first. An empty workflowID
// lists every batch. limit <= 0 means no limit.
func (s *Store) ListBatches(workflowID string, limit int) ([]Batch, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.batchesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []Batch{}, nil
		}
		return nil, err
	}
	out := make([]Batch, 0, len(entries))
	for _, e := range entries {
		id, ok := idFromFilename(e.Name())
		if e.IsDir() || !ok {
			continue
		}
		var b Batch
		if err := readJSONFile(s.batchPath(id), &b); err != nil {
			continue
		}
		if workflowID != "" && b.WorkflowID != workflowID {
			continue
		}
		if b.Items == nil {
			b.Items = []BatchItem{}
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
