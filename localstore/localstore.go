// Package localstore keeps a single user's workflows and run history in a
// .zyvro directory inside the project folder they opened. It exists because
// the desktop app has no database and no accounts: the files on disk are the
// whole state, they diff and merge in git like the rest of the project, and
// the user can read them without us.
package localstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// LocalUserID stands in for the account id the hosted API carries on every
// record. There is exactly one user here, but the frontend types expect the
// field, so it gets a constant rather than an empty string that would read as
// "unknown owner".
const LocalUserID = "local"

// ProjectVersion is the on-disk layout version, written to project.json so a
// future change can migrate instead of guessing.
const ProjectVersion = 1

// ErrNotFound is returned for any id that resolves to no file, including ids
// that are malformed: a caller must not be able to tell a rejected path from
// a missing one, or it could probe the filesystem through the API.
var ErrNotFound = errors.New("not found")

// ErrInvalidID rejects an id before it is ever joined onto a path.
var ErrInvalidID = errors.New("invalid id")

// idPattern is the shape of every id we mint: 24 lowercase hex characters,
// the same shape as the Mongo ObjectIDs the frontend already handles. Ids
// arrive from HTTP requests and become filenames, so nothing outside this
// pattern is allowed anywhere near filepath.Join.
var idPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)

// Workflow is one stored workflow. The JSON tags are the wire shape the
// frontend's Workflow type expects (see Zyvro-frontend/src/lib/api.ts), so a
// file can be handed to the client unchanged. Account-related fields keep
// local defaults: there is one owner and nothing is published from a laptop.
type Workflow struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	Name           string    `json:"name"`
	Slug           string    `json:"slug"`
	Description    string    `json:"description"`
	GraphJSON      string    `json:"graph_json"`
	Visibility     string    `json:"visibility"`
	Version        int       `json:"version"`
	DuplicateCount int       `json:"duplicate_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// WorkflowPatch is a partial update. Every field is a pointer so an explicit
// empty value (clearing a description) is distinguishable from the field not
// being sent at all.
type WorkflowPatch struct {
	Name        *string
	Description *string
	Graph       json.RawMessage
	Visibility  *string
}

// Execution mirrors the hosted WorkflowExecution wire shape the frontend polls.
type Execution struct {
	ID              string     `json:"id"`
	WorkflowID      string     `json:"workflow_id"`
	UserID          string     `json:"user_id"`
	WorkflowVersion int        `json:"workflow_version"`
	Status          string     `json:"status"`
	InputJSON       string     `json:"input_json"`
	OutputJSON      string     `json:"output_json"`
	Error           string     `json:"error"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

// NodeExecution mirrors the hosted per-node record.
type NodeExecution struct {
	ID          string `json:"id"`
	ExecutionID string `json:"execution_id"`
	NodeID      string `json:"node_id"`
	NodeType    string `json:"node_type"`
	Status      string `json:"status"`
	OutputJSON  string `json:"output_json"`
	// Fingerprint is what the replay cache matches on: a node whose inputs and
	// config hash to the same value as a completed run is replayed instead of
	// re-calling a model, which on a laptop is the difference between an edit
	// loop that costs nothing and one that bills every keystroke.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Log is what a plugin node printed while it ran. Only a pack's Lua nodes
	// produce one: it is the user's own code, so when it misbehaves this is the
	// only account of what it was doing, and it has to survive on the record
	// rather than in the daemon's memory.
	Log        []string   `json:"log,omitempty"`
	Error      string     `json:"error"`
	LatencyMs  int64      `json:"latency_ms"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// Run is one execution and its node records in a single file. The hosted API
// keeps these in two collections, but a run is only ever read as a whole, and
// one file per run means a run can never be half-written across two places.
type Run struct {
	Execution
	Nodes []NodeExecution `json:"nodes"`
}

// Project is the marker file that says a folder has been opened as a Zyvro
// project.
type Project struct {
	Version   int       `json:"version"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// TextProvider is what a text node or the Brain uses when it names no
	// provider of its own. It lives in the project file rather than the
	// environment so a project that is meant to run on the local CLI keeps
	// running on it after the user reopens the folder.
	TextProvider string `json:"text_provider,omitempty"`
}

// DefaultTextProvider is what a new local project runs text nodes on. A desktop
// user's most likely working setup is the agent CLI they are already signed in
// to, not an API key they have yet to paste anywhere.
const DefaultTextProvider = "claude-cli"

// Store is a workflow store rooted at a project folder. Every method takes the
// lock: the HTTP daemon serves requests concurrently, and two writers landing
// on the same file would otherwise interleave.
type Store struct {
	Root string

	mu sync.RWMutex
}

// Open prepares the .zyvro directory inside root, creating it on first use so
// that opening any folder just works.
func Open(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve project folder: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("open project folder: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("project path %s is not a directory", abs)
	}
	s := &Store{Root: abs}
	for _, dir := range []string{s.ZyvroDir(), s.workflowsDir(), s.runsDir(), s.MediaDir(), s.PacksDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := s.ensureProjectFile(); err != nil {
		return nil, err
	}
	if err := s.ensureGitignore(); err != nil {
		return nil, err
	}
	return s, nil
}

// gitignoreBody excludes the two things in .zyvro that must not reach a
// repository. The rest of the directory is the point of the feature: workflows
// are meant to be committed and reviewed alongside the code they generate.
const gitignoreBody = `# Written by Zyvro. The rest of .zyvro/ is meant to be committed.

# Provider credentials, stored in plaintext for this local single-user tool.
secrets.json

# Generated media and run history: machine output, often large, always
# reproducible by running the workflow again.
media/
runs/
`

// ensureGitignore writes .zyvro/.gitignore once. It is never rewritten: the
// user may have adjusted it (to commit their run history, say), and silently
// reverting that on the next start would be worse than a stale file.
func (s *Store) ensureGitignore() error {
	path := filepath.Join(s.ZyvroDir(), ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(gitignoreBody), 0o644)
}

// ZyvroDir is the .zyvro directory holding everything this store owns.
func (s *Store) ZyvroDir() string     { return filepath.Join(s.Root, ".zyvro") }
func (s *Store) workflowsDir() string { return filepath.Join(s.ZyvroDir(), "workflows") }
func (s *Store) runsDir() string      { return filepath.Join(s.ZyvroDir(), "runs") }

// MediaDir holds images produced by runs. It is served read-only over /content.
func (s *Store) MediaDir() string { return filepath.Join(s.ZyvroDir(), "media") }

// PacksDir holds the node packs this project carries, one directory each. It is
// created empty when a project is opened rather than on first use, because a
// person who has been told they can install a pack needs somewhere to put it,
// and a path that does not exist yet is not an answer. Unlike media/ and runs/
// it is not in the generated .gitignore: a pack is source, and a workflow that
// depends on one is only reproducible for a colleague if the pack came with it.
func (s *Store) PacksDir() string { return filepath.Join(s.ZyvroDir(), "packs") }

func (s *Store) ensureProjectFile() error {
	path := filepath.Join(s.ZyvroDir(), "project.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	p := Project{
		Version:      ProjectVersion,
		Name:         filepath.Base(s.Root),
		CreatedAt:    time.Now().UTC(),
		TextProvider: DefaultTextProvider,
	}
	return writeJSONAtomic(path, p)
}

// Project reads the project marker. A project written before a field existed
// gets the default rather than an empty value, so an older .zyvro keeps working.
func (s *Store) Project() (*Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var p Project
	if err := readJSONFile(filepath.Join(s.ZyvroDir(), "project.json"), &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.TextProvider) == "" {
		p.TextProvider = DefaultTextProvider
	}
	return &p, nil
}

// ---------- workflows ----------

// List returns every workflow, most recently updated first, which is the order
// the builder's sidebar shows them in.
func (s *Store) List() ([]Workflow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.workflowsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []Workflow{}, nil
		}
		return nil, err
	}
	out := make([]Workflow, 0, len(entries))
	for _, e := range entries {
		id, ok := idFromFilename(e.Name())
		if e.IsDir() || !ok {
			continue // stray file: a listing must not fail on it
		}
		var wf Workflow
		if err := readJSONFile(s.workflowPath(id), &wf); err != nil {
			continue // unreadable or half-written by something else; skip it
		}
		out = append(out, wf)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Get returns one workflow, or ErrNotFound for a missing or malformed id.
func (s *Store) Get(id string) (*Workflow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.get(id)
}

func (s *Store) get(id string) (*Workflow, error) {
	path, err := s.workflowPathChecked(id)
	if err != nil {
		return nil, err
	}
	var wf Workflow
	if err := readJSONFile(path, &wf); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &wf, nil
}

// Create writes a new workflow. An empty graph becomes an empty React Flow
// document rather than "{}", so the builder can load it without a special case.
func (s *Store) Create(name string, graph json.RawMessage) (*Workflow, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	wf := Workflow{
		ID:          NewID(),
		UserID:      LocalUserID,
		Name:        name,
		Slug:        slugify(name, "workflow"),
		GraphJSON:   normalizeGraph(graph),
		Visibility:  "private",
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
		Description: "",
	}
	if err := writeJSONAtomic(s.workflowPath(wf.ID), wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// Update applies a patch and bumps the version, matching the hosted API: every
// save is a new version, which is what the run history is keyed against.
func (s *Store) Update(id string, patch WorkflowPatch) (*Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	wf, err := s.get(id)
	if err != nil {
		return nil, err
	}
	if patch.Name != nil {
		if n := strings.TrimSpace(*patch.Name); n != "" {
			wf.Name = n
			wf.Slug = slugify(n, wf.Slug)
		}
	}
	if patch.Description != nil {
		wf.Description = strings.TrimSpace(*patch.Description)
	}
	if len(patch.Graph) > 0 {
		wf.GraphJSON = normalizeGraph(patch.Graph)
	}
	if patch.Visibility != nil {
		wf.Visibility = validVisibility(*patch.Visibility)
	}
	wf.Version++
	wf.UpdatedAt = time.Now().UTC()
	if err := writeJSONAtomic(s.workflowPath(wf.ID), *wf); err != nil {
		return nil, err
	}
	return wf, nil
}

// Duplicate copies a workflow into a new one. The copy starts at version 1
// with no history of its own, and the original's duplicate_count is bumped so
// the field means the same thing it does on the hosted API.
func (s *Store) Duplicate(id string) (*Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	src, err := s.get(id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	copied := Workflow{
		ID:          NewID(),
		UserID:      LocalUserID,
		Name:        src.Name + " (copy)",
		Slug:        slugify(src.Name+" copy", "workflow-copy"),
		Description: src.Description,
		GraphJSON:   src.GraphJSON,
		Visibility:  "private",
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := writeJSONAtomic(s.workflowPath(copied.ID), copied); err != nil {
		return nil, err
	}
	// The copy is what the caller asked for, so a failure to update the
	// original's counter must not fail the request; the count is a statistic.
	src.DuplicateCount++
	_ = writeJSONAtomic(s.workflowPath(src.ID), *src)
	return &copied, nil
}

// Delete removes a workflow. Its runs are left alone: they are a record of
// what the machine actually did, and deleting a workflow does not un-do them.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.workflowPathChecked(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// ---------- runs ----------

// SaveRun writes the whole run record. It is called on every node transition
// so a poll of GET /api/executions/{id} sees progress, which is why the write
// is atomic: a poll landing mid-write must never read a truncated file.
func (s *Store) SaveRun(r *Run) error {
	if r == nil {
		return errors.New("nil run")
	}
	if !ValidID(r.ID) {
		return ErrInvalidID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Nodes == nil {
		r.Nodes = []NodeExecution{}
	}
	return writeJSONAtomic(s.runPath(r.ID), *r)
}

// GetRun returns one run with its node records.
func (s *Store) GetRun(id string) (*Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !ValidID(id) {
		return nil, ErrNotFound
	}
	var r Run
	if err := readJSONFile(s.runPath(id), &r); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if r.Nodes == nil {
		r.Nodes = []NodeExecution{}
	}
	return &r, nil
}

// ListRuns returns a workflow's runs, newest first. An empty workflowID lists
// every run. limit <= 0 means no limit.
func (s *Store) ListRuns(workflowID string, limit int) ([]Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.runsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []Run{}, nil
		}
		return nil, err
	}
	out := make([]Run, 0, len(entries))
	for _, e := range entries {
		id, ok := idFromFilename(e.Name())
		if e.IsDir() || !ok {
			continue
		}
		var r Run
		if err := readJSONFile(s.runPath(id), &r); err != nil {
			continue
		}
		if workflowID != "" && r.WorkflowID != workflowID {
			continue
		}
		if r.Nodes == nil {
			r.Nodes = []NodeExecution{}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---------- ids and paths ----------

// NewID mints a 24-character lowercase hex id, the shape the frontend already
// treats as a workflow or execution id.
func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the OS entropy source is gone; there is no
		// safe weaker id to fall back to, so this is fatal to the caller.
		panic("localstore: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ValidID reports whether an id is safe to turn into a filename. Ids reach us
// straight from request paths, so this is the only gate between a URL segment
// and the filesystem.
func ValidID(id string) bool { return idPattern.MatchString(id) }

func (s *Store) workflowPath(id string) string {
	return filepath.Join(s.workflowsDir(), id+".json")
}

func (s *Store) runPath(id string) string {
	return filepath.Join(s.runsDir(), id+".json")
}

// workflowPathChecked is the only way a caller-supplied id becomes a path.
// A malformed id reports ErrNotFound rather than ErrInvalidID at the HTTP
// layer's discretion; here it is explicit so tests can tell them apart.
func (s *Store) workflowPathChecked(id string) (string, error) {
	if !ValidID(id) {
		return "", ErrInvalidID
	}
	return s.workflowPath(id), nil
}

// idFromFilename accepts only "<24 hex>.json", so a stray file dropped into
// the directory can never be listed as a workflow.
func idFromFilename(name string) (string, bool) {
	id := strings.TrimSuffix(name, ".json")
	if id == name || !ValidID(id) {
		return "", false
	}
	return id, true
}

// ---------- atomic file IO ----------

// writeJSONAtomic writes through a temporary file in the same directory and
// renames it into place. A crash or a concurrent reader can then only ever see
// the old file or the new one — never a half-written workflow, which would
// lose the user's work with no way to get it back.
func writeJSONAtomic(path string, v any) error {
	return writeJSONAtomicMode(path, v, 0o644)
}

// writeJSONAtomicMode is writeJSONAtomic with an explicit file mode, for the
// secrets file, which must never be group- or world-readable. The mode is set
// on the temp file before the rename so the finished file is never briefly
// readable at the wrong permissions.
func writeJSONAtomicMode(path string, v any, mode os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Any failure past this point leaves a temp file behind, so every exit
	// removes it; on success the rename has already consumed the name and the
	// remove is a no-op.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// fsync before the rename: without it the rename can land while the bytes
	// are still only in the page cache, and a power loss leaves an empty file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// normalizeGraph keeps the stored graph a valid React Flow document. The
// frontend sends the graph as JSON and reads it back as a string, so it is
// stored verbatim rather than re-encoded through a Go struct that would drop
// fields the builder added.
func normalizeGraph(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return `{"nodes":[],"edges":[]}`
	}
	return trimmed
}

func validVisibility(v string) string {
	switch v {
	case "private", "unlisted", "public":
		return v
	default:
		return "private"
	}
}

// slugify produces the human-readable half of a workflow's identity. Unlike
// the hosted store there is no uniqueness check: nothing is addressed by slug
// locally, it exists so the field the frontend reads is not empty.
func slugify(s, fallback string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return fallback
	}
	return out
}
