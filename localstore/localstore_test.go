package localstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestOpenCreatesLayout(t *testing.T) {
	s := newStore(t)
	for _, dir := range []string{"workflows", "runs", "media"} {
		if _, err := os.Stat(filepath.Join(s.ZyvroDir(), dir)); err != nil {
			t.Errorf("expected .zyvro/%s to exist: %v", dir, err)
		}
	}
	p, err := s.Project()
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if p.Version != ProjectVersion {
		t.Errorf("project version = %d, want %d", p.Version, ProjectVersion)
	}
}

func TestWorkflowRoundTrip(t *testing.T) {
	s := newStore(t)

	wf, err := s.Create("My First Flow", json.RawMessage(`{"nodes":[],"edges":[]}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !ValidID(wf.ID) {
		t.Fatalf("Create minted an id that is not 24 hex chars: %q", wf.ID)
	}
	if wf.Slug != "my-first-flow" || wf.Version != 1 || wf.Visibility != "private" {
		t.Errorf("unexpected defaults: %+v", wf)
	}

	got, err := s.Get(wf.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "My First Flow" {
		t.Errorf("Get name = %q", got.Name)
	}

	desc := "does a thing"
	updated, err := s.Update(wf.ID, WorkflowPatch{
		Description: &desc,
		Graph:       json.RawMessage(`{"nodes":[{"id":"n1"}],"edges":[]}`),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Description != desc {
		t.Errorf("description not applied: %+v", updated)
	}
	if updated.Version != 2 {
		t.Errorf("version = %d, want 2", updated.Version)
	}
	if !strings.Contains(updated.GraphJSON, `"n1"`) {
		t.Errorf("graph not applied: %q", updated.GraphJSON)
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != wf.ID {
		t.Fatalf("List = %+v", list)
	}

	if err := s.Delete(wf.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(wf.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	list, _ = s.List()
	if len(list) != 0 {
		t.Errorf("List after Delete = %+v", list)
	}
}

// An empty graph has to become a usable React Flow document, or the builder
// loads a workflow it cannot render.
func TestCreateNormalizesEmptyGraph(t *testing.T) {
	s := newStore(t)
	wf, err := s.Create("empty", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if wf.GraphJSON != `{"nodes":[],"edges":[]}` {
		t.Errorf("graph = %q", wf.GraphJSON)
	}
}

func TestCreateRejectsEmptyName(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create("   ", nil); err == nil {
		t.Fatal("expected an error for a blank name")
	}
}

// List sorts newest-updated first, which is the order the sidebar shows.
func TestListSortedByUpdatedAtDesc(t *testing.T) {
	s := newStore(t)
	first, _ := s.Create("first", nil)
	second, _ := s.Create("second", nil)
	// Touch the older one so it has to come back on top.
	name := "first again"
	if _, err := s.Update(first.ID, WorkflowPatch{Name: &name}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("unexpected order: %+v", list)
	}
}

// A malicious id must be refused before it reaches the filesystem, and must
// not leave anything behind anywhere.
func TestMaliciousIDsRejected(t *testing.T) {
	s := newStore(t)
	bad := []string{
		"../../etc/passwd",
		"..",
		".",
		"",
		"/etc/passwd",
		"abc123",                    // too short
		"0123456789abcdef0123456",   // 23 chars
		"0123456789abcdef012345678", // 25 chars
		"0123456789ABCDEF01234567",  // uppercase
		"0123456789abcdef0123456/",  // separator smuggled in
		"0123456789abcdef0123456\x00",
		"....//....//etc/passwd",
	}
	for _, id := range bad {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			if ValidID(id) {
				t.Fatalf("ValidID accepted %q", id)
			}
			if _, err := s.Get(id); err == nil {
				t.Errorf("Get(%q) returned no error", id)
			}
			if _, err := s.Update(id, WorkflowPatch{}); err == nil {
				t.Errorf("Update(%q) returned no error", id)
			}
			if err := s.Delete(id); err == nil {
				t.Errorf("Delete(%q) returned no error", id)
			}
			if _, err := s.GetRun(id); err == nil {
				t.Errorf("GetRun(%q) returned no error", id)
			}
			if err := s.SaveRun(&Run{Execution: Execution{ID: id}}); err == nil {
				t.Errorf("SaveRun(%q) returned no error", id)
			}
		})
	}

	// Nothing was written: the workflows and runs directories are still empty,
	// and no stray file landed next to the project root.
	for _, dir := range []string{filepath.Join(s.ZyvroDir(), "workflows"), filepath.Join(s.ZyvroDir(), "runs")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir %s: %v", dir, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s is not empty: %v", dir, names(entries))
		}
	}
	rootEntries, _ := os.ReadDir(s.Root)
	if len(rootEntries) != 1 || rootEntries[0].Name() != ".zyvro" {
		t.Errorf("project root gained files: %v", names(rootEntries))
	}
}

// A workflow file survives being listed under a name that is not an id, and a
// stray file never appears as a workflow.
func TestListIgnoresStrayFiles(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create("real", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, name := range []string{"README.md", "notes.json", ".DS_Store"} {
		if err := os.WriteFile(filepath.Join(s.ZyvroDir(), "workflows", name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write stray: %v", err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("List = %d entries, want 1", len(list))
	}
}

// The atomic write must clean up after itself: a leftover .tmp would show up
// in the user's git status and, worse, suggest the write half-failed.
func TestAtomicWriteLeavesNoTmp(t *testing.T) {
	s := newStore(t)
	wf, err := s.Create("temp check", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	name := "temp check again"
	for i := 0; i < 5; i++ {
		if _, err := s.Update(wf.ID, WorkflowPatch{Name: &name}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	if err := s.SaveRun(&Run{Execution: Execution{ID: NewID(), WorkflowID: wf.ID, Status: "completed"}}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	var leftovers []string
	err = filepath.Walk(s.ZyvroDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.Contains(info.Name(), ".tmp") {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestRunRoundTrip(t *testing.T) {
	s := newStore(t)
	wfID := NewID()
	run := &Run{
		Execution: Execution{ID: NewID(), WorkflowID: wfID, Status: "running"},
		Nodes: []NodeExecution{
			{ID: NewID(), NodeID: "n1", NodeType: "textInput", Status: "completed"},
		},
	}
	if err := s.SaveRun(run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	// Saving again is how progress is reported, so it has to overwrite cleanly.
	run.Status = "completed"
	if err := s.SaveRun(run); err != nil {
		t.Fatalf("SaveRun (update): %v", err)
	}

	got, err := s.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != "completed" || len(got.Nodes) != 1 || got.Nodes[0].NodeID != "n1" {
		t.Fatalf("GetRun = %+v", got)
	}

	other := &Run{Execution: Execution{ID: NewID(), WorkflowID: NewID(), Status: "failed"}}
	if err := s.SaveRun(other); err != nil {
		t.Fatalf("SaveRun other: %v", err)
	}
	runs, err := s.ListRuns(wfID, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("ListRuns(%s) = %+v", wfID, runs)
	}
	all, _ := s.ListRuns("", 0)
	if len(all) != 2 {
		t.Errorf("ListRuns(all) = %d, want 2", len(all))
	}
	limited, _ := s.ListRuns("", 1)
	if len(limited) != 1 {
		t.Errorf("ListRuns(limit 1) = %d", len(limited))
	}
}

func TestGetRunNotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.GetRun(NewID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRun = %v, want ErrNotFound", err)
	}
}

// Every workflow file must be complete JSON no matter how many writers were
// racing, which is what the atomic rename buys. Run under -race.
func TestConcurrentWritesDoNotCorrupt(t *testing.T) {
	s := newStore(t)

	const workflows = 4
	ids := make([]string, workflows)
	for i := range ids {
		wf, err := s.Create(fmt.Sprintf("flow %d", i), nil)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids[i] = wf.ID
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 256)
	for i, id := range ids {
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func(id string, i, w int) {
				defer wg.Done()
				name := fmt.Sprintf("flow %d writer %d", i, w)
				graph := json.RawMessage(fmt.Sprintf(`{"nodes":[{"id":"n%d"}],"edges":[]}`, w))
				if _, err := s.Update(id, WorkflowPatch{Name: &name, Graph: graph}); err != nil {
					errCh <- err
				}
			}(id, i, w)
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				if _, err := s.Get(id); err != nil {
					errCh <- err
				}
				if _, err := s.List(); err != nil {
					errCh <- err
				}
			}(id)
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				r := &Run{Execution: Execution{ID: NewID(), WorkflowID: id, Status: "completed"}}
				if err := s.SaveRun(r); err != nil {
					errCh <- err
				}
				if _, err := s.ListRuns(id, 5); err != nil {
					errCh <- err
				}
			}(id)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent operation failed: %v", err)
	}

	// Every file on disk still parses, and the version counter saw every write.
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != workflows {
		t.Fatalf("List = %d workflows, want %d", len(list), workflows)
	}
	for _, wf := range list {
		if wf.Version != 9 { // 1 from Create plus 8 updates
			t.Errorf("workflow %s version = %d, want 9", wf.ID, wf.Version)
		}
		var g map[string]any
		if err := json.Unmarshal([]byte(wf.GraphJSON), &g); err != nil {
			t.Errorf("workflow %s graph is not valid JSON: %v", wf.ID, err)
		}
	}
}

func TestMediaStoreSaveAndURL(t *testing.T) {
	s := newStore(t)
	m := s.Media()
	url, err := m.SaveMedia("exec1", "n1", "out.png", []byte("png-bytes"), "image/png")
	if err != nil {
		t.Fatalf("SaveMedia: %v", err)
	}
	if !strings.HasPrefix(url, "/content/") {
		t.Fatalf("url = %q, want a /content/ path", url)
	}
	name := strings.TrimPrefix(url, "/content/")
	data, err := os.ReadFile(filepath.Join(s.MediaDir(), name))
	if err != nil {
		t.Fatalf("read saved media: %v", err)
	}
	if string(data) != "png-bytes" {
		t.Errorf("saved bytes = %q", data)
	}
}

// Media names are built from ids and filenames we did not write, so nothing in
// them may survive as a path separator or a "..".
func TestMediaStoreNeutralizesTraversal(t *testing.T) {
	s := newStore(t)
	m := s.Media()
	url, err := m.SaveMedia("../../etc", "../..", "../../../passwd.png", []byte("x"), "image/png")
	if err != nil {
		t.Fatalf("SaveMedia: %v", err)
	}
	name := strings.TrimPrefix(url, "/content/")
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		t.Fatalf("media name escaped sanitization: %q", name)
	}
	if _, err := os.Stat(filepath.Join(s.MediaDir(), name)); err != nil {
		t.Errorf("file was not written inside the media dir: %v", err)
	}
}

func TestNewIDIsUniqueAndValid(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if !ValidID(id) {
			t.Fatalf("NewID produced %q", id)
		}
		if seen[id] {
			t.Fatalf("NewID repeated %q", id)
		}
		seen[id] = true
	}
}

// The gitignore is written on Open and is what keeps a plaintext key out of
// the user's repository.
func TestOpenWritesGitignore(t *testing.T) {
	s := newStore(t)
	path := filepath.Join(s.ZyvroDir(), ".gitignore")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(body), "secrets.json") {
		t.Errorf(".gitignore does not exclude secrets.json:\n%s", body)
	}

	// Reopening never clobbers what the user edited.
	if err := os.WriteFile(path, []byte("# mine\nsecrets.json\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Open(s.Root); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != "# mine\nsecrets.json\n" {
		t.Errorf(".gitignore was rewritten:\n%s", again)
	}
}

func TestProjectDefaultsToLocalCLI(t *testing.T) {
	s := newStore(t)
	p, err := s.Project()
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if p.TextProvider != DefaultTextProvider {
		t.Errorf("TextProvider = %q, want %q", p.TextProvider, DefaultTextProvider)
	}

	// A project.json written before the field existed still answers usefully.
	if err := os.WriteFile(filepath.Join(s.ZyvroDir(), "project.json"), []byte(`{"version":1,"name":"old"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	p, err = s.Project()
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if p.TextProvider != DefaultTextProvider {
		t.Errorf("legacy project TextProvider = %q, want the default", p.TextProvider)
	}
}

func TestSecretsRoundTrip(t *testing.T) {
	s := newStore(t)

	if got, err := s.ListSecrets(); err != nil || len(got) != 0 {
		t.Fatalf("ListSecrets on a fresh project = %+v, %v", got, err)
	}

	sec, err := s.SetSecret("anthropic", "sk-ant-secret-4321")
	if err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if sec.Provider != "anthropic" || sec.SecretLast4 != "4321" {
		t.Errorf("secret view = %+v", sec)
	}

	values, err := s.SecretValues()
	if err != nil {
		t.Fatalf("SecretValues: %v", err)
	}
	if values["anthropic"] != "sk-ant-secret-4321" {
		t.Errorf("SecretValues = %+v", values)
	}

	// Replacing a key keeps exactly one entry.
	if _, err := s.SetSecret("anthropic", "sk-ant-secret-0000"); err != nil {
		t.Fatalf("SetSecret (replace): %v", err)
	}
	list, _ := s.ListSecrets()
	if len(list) != 1 || list[0].SecretLast4 != "0000" {
		t.Fatalf("after replace: %+v", list)
	}

	if err := s.DeleteSecret("anthropic"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if list, _ := s.ListSecrets(); len(list) != 0 {
		t.Errorf("after delete: %+v", list)
	}
	// Deleting one that was never there is not an error: it is already gone.
	if err := s.DeleteSecret("openai"); err != nil {
		t.Errorf("DeleteSecret on a missing provider = %v", err)
	}
}

// Plaintext on disk is the deliberate choice; 0600 is the part that is not
// optional.
func TestSecretsFileMode(t *testing.T) {
	s := newStore(t)
	if _, err := s.SetSecret("openai", "sk-abcd"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.ZyvroDir(), "secrets.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestSecretsRejectBadProviders(t *testing.T) {
	s := newStore(t)
	for _, provider := range []string{"", "..", "../../etc/passwd", "Anthropic!", strings.Repeat("a", 41)} {
		if _, err := s.SetSecret(provider, "x"); !errors.Is(err, ErrInvalidProvider) {
			t.Errorf("SetSecret(%q) = %v, want ErrInvalidProvider", provider, err)
		}
		if err := s.DeleteSecret(provider); !errors.Is(err, ErrInvalidProvider) {
			t.Errorf("DeleteSecret(%q) = %v, want ErrInvalidProvider", provider, err)
		}
	}
	if _, err := s.SetSecret("openai", "   "); err == nil {
		t.Error("an empty secret should be refused")
	}
}

func TestDuplicate(t *testing.T) {
	s := newStore(t)
	src, err := s.Create("Original", json.RawMessage(`{"nodes":[{"id":"n1"}],"edges":[]}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	copied, err := s.Duplicate(src.ID)
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if copied.ID == src.ID || copied.Version != 1 || copied.GraphJSON != src.GraphJSON {
		t.Fatalf("copy = %+v", copied)
	}
	after, _ := s.Get(src.ID)
	if after.DuplicateCount != 1 {
		t.Errorf("duplicate_count = %d, want 1", after.DuplicateCount)
	}
	if _, err := s.Duplicate("../../etc/passwd"); err == nil {
		t.Error("Duplicate accepted a malformed id")
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
