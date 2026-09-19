package localstore

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func batchWith(id string, statuses ...string) *Batch {
	b := &Batch{ID: id, WorkflowID: NewID(), Input: "gfx", Status: "running", CreatedAt: time.Now().UTC()}
	for i, st := range statuses {
		b.Items = append(b.Items, BatchItem{Path: "sprites/" + st + string(rune('a'+i)) + ".png", Status: st})
	}
	return b
}

func TestBatchRoundTrip(t *testing.T) {
	s := newStore(t)
	b := batchWith(NewID(), "completed", "failed", "pending")
	if err := s.SaveBatch(b); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetBatch(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Input != "gfx" || len(got.Items) != 3 {
		t.Fatalf("batch = %+v", got)
	}
	if _, err := s.GetBatch(NewID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing batch = %v, want ErrNotFound", err)
	}
}

// The one-line summary a history row shows. Derived from the items on every
// read rather than stored, so it cannot drift from them — the failure it
// avoids is a row that says "519 done" over a batch where three did not.
func TestBatchCountsComeFromTheItems(t *testing.T) {
	b := batchWith(NewID(), "completed", "completed", "failed", "running", "pending", "cancelled")
	c := b.Counts()
	if c.Total != 6 || c.Completed != 2 || c.Failed != 1 || c.Running != 1 || c.Pending != 1 || c.Cancelled != 1 {
		t.Errorf("counts = %+v", c)
	}
	if empty := (&Batch{}).Counts(); empty.Total != 0 {
		t.Errorf("empty batch counts = %+v", empty)
	}
}

func TestListBatchesFiltersByWorkflowAndOrdersNewestFirst(t *testing.T) {
	s := newStore(t)
	mine, other := NewID(), NewID()

	older := batchWith(NewID(), "completed")
	older.WorkflowID = mine
	older.CreatedAt = time.Now().UTC().Add(-time.Hour)
	newer := batchWith(NewID(), "completed")
	newer.WorkflowID = mine
	newer.CreatedAt = time.Now().UTC()
	theirs := batchWith(NewID(), "completed")
	theirs.WorkflowID = other

	for _, b := range []*Batch{older, newer, theirs} {
		if err := s.SaveBatch(b); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	list, err := s.ListBatches(mine, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].ID != newer.ID || list[1].ID != older.ID {
		t.Fatalf("list = %v", list)
	}
	if all, _ := s.ListBatches("", 0); len(all) != 3 {
		t.Errorf("unfiltered list = %d, want 3", len(all))
	}
	if limited, _ := s.ListBatches(mine, 1); len(limited) != 1 || limited[0].ID != newer.ID {
		t.Errorf("limited list = %v", limited)
	}
}

// Batches live inside runs/ because a batch is run history and the generated
// .gitignore already excludes runs/. The price of that is this: the run
// listing walks the same folder, so it has to ignore the directory. It does —
// but a change to either side would break the other silently, which is what
// this pins.
func TestBatchesAreInvisibleToTheRunListing(t *testing.T) {
	s := newStore(t)
	run := &Run{Execution: Execution{ID: NewID(), WorkflowID: NewID(), Status: "completed", CreatedAt: time.Now().UTC()}}
	if err := s.SaveRun(run); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := s.SaveBatch(batchWith(NewID(), "completed")); err != nil {
		t.Fatalf("save batch: %v", err)
	}

	runs, err := s.ListRuns("", 0)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("runs = %v, want just the one run", runs)
	}
	if dir := filepath.Base(s.batchesDir()); dir != "batches" {
		t.Errorf("batches directory = %q", dir)
	}
}

// A run started by a batch carries its id. That field is the whole of the
// second arbitrage: 519 runs stay 519 runs, and one line stands for them,
// without a second history that has to be kept in step with the first.
func TestARunRemembersItsBatch(t *testing.T) {
	s := newStore(t)
	batchID := NewID()
	run := &Run{Execution: Execution{ID: NewID(), WorkflowID: NewID(), BatchID: batchID, Status: "completed", CreatedAt: time.Now().UTC()}}
	if err := s.SaveRun(run); err != nil {
		t.Fatalf("save run: %v", err)
	}
	got, err := s.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.BatchID != batchID {
		t.Errorf("batch_id = %q, want %q", got.BatchID, batchID)
	}
}
