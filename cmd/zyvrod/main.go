// Command zyvrod is the local backend for the Zyvro desktop app. It serves the
// same HTTP API shape as cmd/server, but for one person on one machine: the
// project folder they opened is the database, there are no accounts, and the
// only thing standing between the daemon and every other process on the box is
// a token handed to the parent process over stdout.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Zyvro/Zyvro-engine/engine"
	"github.com/Zyvro/Zyvro-engine/localstore"
	"github.com/Zyvro/Zyvro-engine/plugins"
	"github.com/Zyvro/Zyvro-engine/providers"
)

// executionTimeout bounds a single run. There is no queue and no operator to
// notice a wedged graph, so the run has to give up on its own.
const executionTimeout = 30 * time.Minute

func main() {
	project := flag.String("project", "", "project folder to open (required)")
	port := flag.Int("port", 0, "port to listen on; 0 picks a free one")
	showVersion := flag.Bool("version", false, "print the engine version and exit")
	nodeTypes := flag.Bool("node-types", false, "print the node types this engine runs, one per line, and exit")
	inputTypes := flag.Bool("runtime-input-types", false, "print the node types a run can fill by name, one per line, and exit")
	flag.Parse()

	// --version answers before anything else is checked. It is what an updater
	// or a support request runs on a binary it found on disk, and that binary
	// has no project folder to open.
	if *showVersion {
		fmt.Println(versionLine())
		return
	}

	// --node-types answers before the project check too, for the same reason:
	// the question is about the binary, not about anyone's folder. It exists so
	// a build can compare this list against a palette maintained by hand
	// somewhere else, which is otherwise a mirror nobody checks.
	//
	// Only the types this binary implements itself. A project's installed packs
	// add more, and those are not a property of the engine.
	if *nodeTypes {
		for _, t := range engine.RunnableBuiltinNodeTypes() {
			fmt.Println(t)
		}
		return
	}

	// --runtime-input-types, pour la même raison : l'éditeur décide s'il propose
	// un nom à un nœud, et c'est ce binaire qui décide si ce nom sert à quelque
	// chose. Un nœud qu'un éditeur laisse sans nom est un nœud qu'aucune
	// exécution ne peut remplir autrement qu'en désignant son identifiant.
	if *inputTypes {
		for _, t := range engine.RuntimeInputTypes {
			fmt.Println(t)
		}
		return
	}

	if strings.TrimSpace(*project) == "" {
		fatal("--project is required")
	}
	store, err := localstore.Open(*project)
	if err != nil {
		fatal("open project: %v", err)
	}

	d, err := newDaemon(store)
	if err != nil {
		fatal("start daemon: %v", err)
	}

	// Loopback only. This is someone's laptop: binding 0.0.0.0 would expose a
	// process that can read and write their files to every machine on whatever
	// coffee-shop network they happen to be on.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fatal("listen: %v", err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	srv := &http.Server{
		Handler:           d.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The handshake is the only thing this process ever writes to stdout, so
	// the parent can read exactly one line and know the port and the token.
	if err := d.writeHandshake(os.Stdout, actualPort); err != nil {
		fatal("handshake: %v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		fatal("serve: %v", err)
	case <-stop:
		// Shut down cleanly so an in-flight run gets the chance to persist its
		// last node before the process goes away.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

func fatal(format string, args ...any) {
	// Diagnostics go to stderr: stdout carries the handshake and nothing else.
	fmt.Fprintf(os.Stderr, "zyvrod: "+format+"\n", args...)
	os.Exit(1)
}

// ---------- daemon ----------

type daemon struct {
	store *localstore.Store
	// env is the configuration the process was launched with. It is the
	// fallback under the project's own secrets, never the other way round.
	env   *providers.Config
	token string

	// runs tracks in-flight executions only so a shutdown can wait on nothing
	// more than the goroutines it already started.
	runs sync.WaitGroup

	// mu guards the pack registry and its listing. Both are replaced wholesale
	// by a reload, which can happen while a run is reading the registry.
	mu      sync.RWMutex
	plugins *plugins.Registry
	packs   []packInfo
}

func newDaemon(store *localstore.Store) (*daemon, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	d := &daemon{
		store: store,
		env:   providers.FromEnv(),
		token: token,
	}
	// Packs are read once here rather than lazily on the first run: the palette
	// asks for the node catalogue before anything is ever executed, and a pack
	// that is going to be refused should say so in the startup log, next to the
	// project it was refused for.
	d.loadPacks()
	return d, nil
}

// providerConfig builds the configuration for one run or one catalog listing.
// It is rebuilt every time rather than cached because the user can paste a key
// into the settings panel mid-session and expect the next run to use it.
//
// Precedence is secrets.json first, environment second: the file is what the
// user set through the app, and the environment is whatever the process
// happened to inherit.
func (d *daemon) providerConfig() *providers.Config {
	cfg := *d.env // copy: the launch configuration is never mutated

	if secrets, err := d.store.SecretValues(); err == nil {
		for provider, value := range secrets {
			// `bfl` manquait ici, et c'est le genre d'oubli que cette forme
			// invite : une clé Black Forest Labs se stockait, le panneau
			// affichait « Connected », et l'exécution répondait « no Black
			// Forest Labs key is configured ». Rien ne reliait les deux, parce
			// que ce switch est une seconde liste de fournisseurs à côté du
			// catalogue — c'est ce que `TestEveryKeyProviderReachesTheConfig`
			// compare maintenant, fournisseur par fournisseur.
			switch provider {
			case "google":
				cfg.GoogleAPIKey = value
			case "bfl":
				cfg.BFLAPIKey = value
			case "anthropic":
				cfg.AnthropicAPIKey = value
			case "openai":
				cfg.OpenAIAPIKey = value
			case "ollama":
				cfg.OllamaAPIKey = value
			case "claude-cli":
				cfg.ClaudeCLIPath = value
			case "codex-cli":
				cfg.CodexCLIPath = value
			case "qwen-cli":
				cfg.QwenCLIPath = value
			}
		}
	}

	// The CLI providers run as subprocesses, so they run where the user's files
	// are: a workflow that asks the CLI to read the project only works if the
	// CLI was started inside it.
	cfg.LocalCLIWorkdir = d.store.Root

	if p, err := d.store.Project(); err == nil && p.TextProvider != "" {
		cfg.TextProvider = p.TextProvider
	}

	// The order the project chose, and the servers it talks to. Both were being
	// saved and neither was reaching the engine: the panel showed a list runs
	// did not follow.
	cfg.Preference = providers.Preference(d.store.ProviderOrder())
	if stored := d.store.ProviderEndpoints(); len(stored) > 0 {
		cfg.Endpoints = map[string]providers.Endpoint{}
		for id, e := range stored {
			cfg.Endpoints[id] = providers.Endpoint{URL: e.URL, Key: e.Key, Model: e.Model}
		}
	}
	return &cfg
}

// newToken mints the bearer token for this process. It lives only in memory
// and in the handshake line, so a token cannot outlive the daemon that issued
// it or be recovered from disk later.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// handshake is the startup contract with the parent process. It is a struct
// rather than a map so the field order stays the documented one, which makes
// the line readable in a log without reformatting it.
// Version is appended rather than inserted, and every field that was here
// before is still here: the desktop reads this line from a daemon it did not
// necessarily build, and treats a missing "version" as an engine too old to
// report one. Removing or renaming a field would break the app against every
// engine already installed.
type handshake struct {
	Ready   bool   `json:"ready"`
	Port    int    `json:"port"`
	Token   string `json:"token"`
	Project string `json:"project"`
	Version string `json:"version"`
}

func (d *daemon) writeHandshake(w *os.File, port int) error {
	line, err := json.Marshal(handshake{
		Ready:   true,
		Port:    port,
		Token:   d.token,
		Project: d.store.Root,
		Version: Version,
	})
	if err != nil {
		return err
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		return err
	}
	// Flush explicitly: the parent blocks on this line, and if stdout is a pipe
	// the kernel buffer alone is not a promise that it was delivered.
	_ = w.Sync()
	return nil
}

// handler builds the route table. Only /api/* sits behind the token: /health
// is a liveness probe that reveals nothing, and /content is loaded by the
// browser itself from plain <img src> tags, which cannot carry an
// Authorization header — a token there would render every generated image as
// a broken one. What keeps /content safe instead is the traversal check in
// serveMedia, the loopback bind on a random port, and the fact that the media
// directory only ever holds images this project's own runs produced.
func (d *daemon) handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/auth/me", d.authMe)
	api.HandleFunc("GET /api/workflows", d.listWorkflows)
	api.HandleFunc("POST /api/workflows", d.createWorkflow)
	api.HandleFunc("GET /api/workflows/{id}", d.getWorkflow)
	api.HandleFunc("PUT /api/workflows/{id}", d.updateWorkflow)
	api.HandleFunc("DELETE /api/workflows/{id}", d.deleteWorkflow)
	api.HandleFunc("POST /api/workflows/{id}/duplicate", d.duplicateWorkflow)
	api.HandleFunc("POST /api/executions", d.runExecution)
	api.HandleFunc("GET /api/executions/{id}", d.getExecution)
	api.HandleFunc("GET /api/workflows/{id}/executions", d.listWorkflowExecutions)
	api.HandleFunc("GET /api/workflows/{id}/executions/last", d.getLastExecution)
	api.HandleFunc("GET /api/providers", d.listProviders)
	api.HandleFunc("PUT /api/providers/order", d.setProviderOrder)
	api.HandleFunc("PUT /api/providers/{id}/endpoint", d.setProviderEndpoint)
	api.HandleFunc("GET /api/providers/{id}/models", d.providerModels)
	api.HandleFunc("GET /api/secrets", d.listSecrets)
	api.HandleFunc("PUT /api/secrets", d.setSecret)
	api.HandleFunc("DELETE /api/secrets/{provider}", d.deleteSecret)
	api.HandleFunc("POST /api/completion", d.codeCompletion)
	api.HandleFunc("GET /api/ai/quota", d.aiQuota)
	api.HandleFunc("GET /api/local/status", d.localStatus)
	api.HandleFunc("GET /api/nodes", d.listNodes)
	api.HandleFunc("GET /api/packs", d.listPacks)
	api.HandleFunc("POST /api/packs/reload", d.reloadPacks)

	mux := http.NewServeMux()
	mux.Handle("/api/", d.requireToken(api))
	// /mcp is the same tool surface the hosted API serves, so the agent CLI the
	// desktop app runs in the project folder can drive these workflows. It is
	// the same process and the same authority as /api, so it takes the same
	// token rather than a second one of its own.
	mux.Handle("/mcp", d.requireToken(http.HandlerFunc(d.mcpHandler)))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /content/{path...}", d.serveMedia)
	return cors(mux)
}

// requireToken rejects anything that does not present the token minted at
// startup. Without it, any other process on the machine — or a web page that
// guessed the port — could drive the daemon over the user's own files.
func (d *daemon) requireToken(next http.Handler) http.Handler {
	want := []byte("Bearer " + d.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cors keeps the allowed origins to the two that can legitimately exist: the
// Vite dev server (and any other localhost port the app is served from) and
// the packaged Electron window, which sends a file:// or null origin. An
// unrecognized origin gets no header at all, so the browser refuses the
// response rather than us deciding it was probably fine.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			// The shared web client sends every request with credentials:
			// "include", and a browser discards the response of such a request
			// unless this header says true. Echoing a specific origin rather
			// than "*" is what makes that legal, and allowedOrigin has already
			// narrowed it to this machine.
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowedOrigin(origin string) bool {
	if origin == "" {
		return false // same-origin or a non-browser client: nothing to echo
	}
	// Electron sends "file://" for a packaged window and "null" for some
	// sandboxed loads; both are the app itself.
	if origin == "file://" || origin == "null" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// ---------- auth ----------

// authMe answers with a synthetic local user. There are no accounts in local
// mode and nothing here is a credential: this endpoint exists only so the
// Builder component the desktop app reuses from the web app finds the session
// it expects, instead of redirecting to a login page that does not exist.
func (d *daemon) authMe(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(d.store.Root)
	if p, err := d.store.Project(); err == nil && p.Name != "" {
		name = p.Name
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       localstore.LocalUserID,
		"email":    "local@zyvro",
		"name":     name,
		"is_admin": false,
	})
}

// ---------- workflows ----------

func (d *daemon) listWorkflows(w http.ResponseWriter, r *http.Request) {
	wfs, err := d.store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list workflows")
		return
	}
	writeJSON(w, http.StatusOK, wfs)
}

type workflowPayload struct {
	Name string `json:"name"`
	// Description is a pointer so an explicit "" clears it, which a value type
	// could not tell apart from the field being absent.
	Description *string         `json:"description"`
	GraphJSON   json.RawMessage `json:"graph_json"`
	Visibility  *string         `json:"visibility"`
}

func (d *daemon) createWorkflow(w http.ResponseWriter, r *http.Request) {
	var req workflowPayload
	if !readJSON(w, r, &req) {
		return
	}
	wf, err := d.store.Create(req.Name, req.GraphJSON)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Description != nil || req.Visibility != nil {
		wf, err = d.store.Update(wf.ID, localstore.WorkflowPatch{
			Description: req.Description,
			Visibility:  req.Visibility,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to create workflow")
			return
		}
	}
	writeJSON(w, http.StatusCreated, wf)
}

func (d *daemon) getWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, err := d.store.Get(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err, "workflow not found")
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (d *daemon) updateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req workflowPayload
	if !readJSON(w, r, &req) {
		return
	}
	patch := localstore.WorkflowPatch{
		Description: req.Description,
		Graph:       req.GraphJSON,
		Visibility:  req.Visibility,
	}
	if strings.TrimSpace(req.Name) != "" {
		patch.Name = &req.Name
	}
	wf, err := d.store.Update(r.PathValue("id"), patch)
	if err != nil {
		writeStoreErr(w, err, "workflow not found")
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (d *daemon) duplicateWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, err := d.store.Duplicate(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err, "workflow not found")
		return
	}
	writeJSON(w, http.StatusCreated, wf)
}

func (d *daemon) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	if err := d.store.Delete(r.PathValue("id")); err != nil {
		writeStoreErr(w, err, "workflow not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- executions ----------

type runRequest struct {
	WorkflowID string         `json:"workflow_id"`
	Inputs     map[string]any `json:"inputs"`
	// UseCache replays unchanged nodes from the previous successful run
	// instead of re-calling models (default: true).
	UseCache *bool `json:"use_cache"`
	// TargetNodeID runs only that node and its uncached ancestors.
	TargetNodeID string `json:"target_node_id"`
}

// runExecution is POST /api/executions: it starts the run and answers straight
// away with the execution id, so the builder can poll it exactly as it does
// against the hosted API.
func (d *daemon) runExecution(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if !readJSON(w, r, &req) {
		return
	}
	run, err := d.startRun(req)
	if err != nil {
		var re *runError
		if errors.As(err, &re) {
			writeErr(w, re.status, re.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to create execution")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"execution_id": run.ID,
		"status":       "queued",
	})
}

// runError is a refused run that already knows how the HTTP layer should report
// it. The MCP tools start runs through the same function and only need the
// message, so the status travels with the error rather than the caller having
// to rediscover it.
type runError struct {
	status int
	msg    string
}

func (e *runError) Error() string { return e.msg }

// storeRunError collapses a rejected id and a missing file into the same 404,
// the same way writeStoreErr does for the read routes.
func storeRunError(err error, msg string) error {
	if errors.Is(err, localstore.ErrNotFound) || errors.Is(err, localstore.ErrInvalidID) {
		return &runError{http.StatusNotFound, msg}
	}
	return &runError{http.StatusInternalServerError, msg}
}

// startRun validates the graph, writes a queued run record, and hands the work
// to a goroutine. There is no queue: one desktop user means the run can start
// immediately.
//
// This is the only place a run begins. POST /api/executions and the MCP
// zyvro_run_workflow tool both come through here, so the agent in the chat
// panel and the builder cannot end up with two different ideas of what a
// runnable workflow is.
func (d *daemon) startRun(req runRequest) (*localstore.Run, error) {
	if req.WorkflowID == "" {
		return nil, &runError{http.StatusBadRequest, "workflow_id required"}
	}
	wf, err := d.store.Get(req.WorkflowID)
	if err != nil {
		return nil, storeRunError(err, "workflow not found")
	}
	graph, err := engine.ParseGraph(wf.GraphJSON)
	if err != nil {
		return nil, &runError{http.StatusBadRequest, "invalid graph: " + err.Error()}
	}
	if _, err := engine.TopoOrder(graph); err != nil {
		return nil, &runError{http.StatusBadRequest, "graph validation failed: " + err.Error()}
	}

	inputsJSON := "{}"
	if req.Inputs != nil {
		b, _ := json.Marshal(req.Inputs)
		inputsJSON = string(b)
	}
	run := &localstore.Run{
		Execution: localstore.Execution{
			ID:              localstore.NewID(),
			WorkflowID:      wf.ID,
			UserID:          localstore.LocalUserID,
			WorkflowVersion: wf.Version,
			Status:          "queued",
			InputJSON:       inputsJSON,
			CreatedAt:       time.Now().UTC(),
		},
		Nodes: []localstore.NodeExecution{},
	}
	if err := d.store.SaveRun(run); err != nil {
		return nil, &runError{http.StatusInternalServerError, "failed to create execution"}
	}

	useCache := req.UseCache == nil || *req.UseCache
	d.runs.Add(1)
	go func() {
		defer d.runs.Done()
		ctx, cancel := context.WithTimeout(context.Background(), executionTimeout)
		defer cancel()
		d.execute(ctx, run, wf, graph, req.Inputs, useCache, req.TargetNodeID)
	}()

	return run, nil
}

// execute runs the graph and persists per-node status as it goes, so a poll of
// GET /api/executions/{id} sees the same progression the hosted API reports.
func (d *daemon) execute(ctx context.Context, run *localstore.Run, wf *localstore.Workflow, graph *engine.Graph, inputs map[string]any, useCache bool, targetNode string) {
	rec := &runRecorder{store: d.store, run: run}
	rec.update(func(r *localstore.Run) {
		r.Status = "running"
		r.StartedAt = ptrTime(time.Now().UTC())
	})

	rt := engine.NewRuntime(run.ID, graph, d.providerConfig(), d.store.Media(), inputs)
	// The whole reason the desktop app exists: the engine runs where the user's
	// files are, so the file nodes get the opened project folder to work in.
	rt.Files = d.store.Files()
	// The registry is read once for the whole run. A pack reloaded halfway
	// through would leave the graph half-executed against one set of node
	// definitions and half against another.
	rt.Plugins = d.pluginRegistry()

	// Fingerprints of the current graph state; without them nothing can be
	// replayed, so a failure here only costs cache hits, not the run.
	//
	// Asked of the runtime rather than of the package, because the runtime
	// holds the registry: here, where packs are installed and their scripts
	// are files the user can edit, a node's code has to be part of what
	// identifies its result.
	fps, _, fpErr := rt.ComputeFingerprints(inputs)
	if fpErr == nil {
		rt.Fingerprints = fps
		if useCache {
			rt.Cache = d.replayCache(wf.ID)
		}
	}

	// A single-node run executes only the target and whichever ancestors are
	// not already cached. The target itself is never replayed: the user asked
	// for it to run again.
	sub := graph
	if targetNode != "" {
		var subErr error
		sub, subErr = engine.SubgraphForNode(graph, targetNode)
		if subErr != nil {
			rec.finishFailed(subErr.Error())
			return
		}
		rt.Graph = sub
		delete(rt.Cache, targetNode)
	}
	rec.fingerprints = rt.Fingerprints

	if err := rt.ExecuteWithReporting(ctx, rec); err != nil {
		msg := err.Error()
		status := "failed"
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			status, msg = "cancelled", "cancelled"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			msg = "execution timed out after " + executionTimeout.String()
		}
		rec.finishWith(status, msg)
		return
	}
	rec.markReplayed(rt.Replayed)

	// The workflow's result is what its output and preview nodes hold; a graph
	// with neither falls back to the last node so a simple chain still shows
	// something.
	result := map[string]any{}
	for _, n := range sub.Nodes {
		if n.Type != "output" && n.Type != "preview" {
			continue
		}
		if o, ok := rt.Outputs[n.ID]; ok {
			result[n.ID] = map[string]any{"type": o.Type, "value": o.Value}
		}
	}
	if len(result) == 0 {
		order, _ := engine.TopoOrder(sub)
		for i := len(order) - 1; i >= 0; i-- {
			if o, ok := rt.Outputs[order[i]]; ok {
				result[order[i]] = map[string]any{"type": o.Type, "value": o.Value}
				break
			}
		}
	}
	out, _ := json.Marshal(result)
	rec.update(func(r *localstore.Run) {
		r.Status = "completed"
		r.OutputJSON = sanitizeOutput(string(out))
		r.FinishedAt = ptrTime(time.Now().UTC())
	})
}

// replayCache loads the node outputs of the most recent successful run so an
// edit to one node does not re-bill every node upstream of it.
func (d *daemon) replayCache(workflowID string) map[string]engine.CacheEntry {
	runs, err := d.store.ListRuns(workflowID, 5)
	if err != nil {
		return nil
	}
	for _, r := range runs {
		if r.Status != "completed" {
			continue
		}
		cached := make([]engine.CachedNodeResult, 0, len(r.Nodes))
		for _, n := range r.Nodes {
			cached = append(cached, engine.CachedNodeResult{
				NodeID:      n.NodeID,
				ExecutionID: n.ExecutionID,
				Status:      n.Status,
				Fingerprint: n.Fingerprint,
				OutputJSON:  n.OutputJSON,
			})
		}
		return engine.LoadCache(cached)
	}
	return nil
}

// runRecorder is the engine's NodeReporter. It keeps the run in memory and
// rewrites the whole file on every transition, which is cheap for one user and
// means a reader never has to stitch two files together to see progress.
type runRecorder struct {
	store        *localstore.Store
	fingerprints map[string]engine.NodeFingerprint

	mu  sync.Mutex
	run *localstore.Run
}

func (rr *runRecorder) update(fn func(*localstore.Run)) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	fn(rr.run)
	if err := rr.store.SaveRun(rr.run); err != nil {
		log.Printf("run %s: persist failed: %v", rr.run.ID, err)
	}
}

func (rr *runRecorder) finishFailed(msg string) { rr.finishWith("failed", msg) }

func (rr *runRecorder) finishWith(status, msg string) {
	rr.update(func(r *localstore.Run) {
		r.Status = status
		r.Error = msg
		r.FinishedAt = ptrTime(time.Now().UTC())
	})
}

// NodeStart records a node beginning and returns the closure that records how
// it ended, matching engine.NodeReporter.
func (rr *runRecorder) NodeStart(nodeID, nodeType string) func(*engine.NodeOutput, error) {
	id := localstore.NewID()
	started := time.Now()
	rr.update(func(r *localstore.Run) {
		r.Nodes = append(r.Nodes, localstore.NodeExecution{
			ID:          id,
			ExecutionID: r.ID,
			NodeID:      nodeID,
			NodeType:    nodeType,
			Status:      "running",
			Fingerprint: string(rr.fingerprints[nodeID]),
			StartedAt:   ptrTime(started.UTC()),
		})
	})
	return func(out *engine.NodeOutput, err error) {
		latency := time.Since(started).Milliseconds()
		rr.update(func(r *localstore.Run) {
			n := findNode(r, id)
			if n == nil {
				return
			}
			n.LatencyMs = latency
			n.FinishedAt = ptrTime(time.Now().UTC())
			if err != nil {
				n.Status = "failed"
				n.Error = err.Error()
				return
			}
			n.Status = "completed"
			if out != nil {
				b, mErr := json.Marshal(out)
				if mErr == nil {
					n.OutputJSON = sanitizeOutput(string(b))
				}
			}
		})
	}
}

// NodeLog implements engine.NodeLogger. What a plugin node printed goes onto
// that node's record, next to its error, because the two are read together: the
// error says the node failed and the log says what it thought it was doing.
//
// The last matching record is the one that gets it. A node id can appear more
// than once in a run when the Brain calls a node as a tool, and the record still
// running is always the most recent.
func (rr *runRecorder) NodeLog(nodeID string, lines []string) {
	if len(lines) == 0 {
		return
	}
	rr.update(func(r *localstore.Run) {
		for i := len(r.Nodes) - 1; i >= 0; i-- {
			if r.Nodes[i].NodeID == nodeID {
				r.Nodes[i].Log = append(r.Nodes[i].Log, lines...)
				return
			}
		}
	})
}

// markReplayed flags the nodes served from the cache so the builder can show
// them as reused rather than as work that just happened.
func (rr *runRecorder) markReplayed(replayed map[string]bool) {
	if len(replayed) == 0 {
		return
	}
	rr.update(func(r *localstore.Run) {
		for i := range r.Nodes {
			if replayed[r.Nodes[i].NodeID] && r.Nodes[i].Status == "completed" {
				r.Nodes[i].Status = "cached"
			}
		}
	})
}

func findNode(r *localstore.Run, id string) *localstore.NodeExecution {
	for i := range r.Nodes {
		if r.Nodes[i].ID == id {
			return &r.Nodes[i]
		}
	}
	return nil
}

func ptrTime(t time.Time) *time.Time { return &t }

func (d *daemon) getExecution(w http.ResponseWriter, r *http.Request) {
	run, err := d.store.GetRun(r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err, "execution not found")
		return
	}
	writeJSON(w, http.StatusOK, executionResponse(run))
}

func (d *daemon) listWorkflowExecutions(w http.ResponseWriter, r *http.Request) {
	runs, err := d.store.ListRuns(r.PathValue("id"), 20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list executions")
		return
	}
	execs := make([]localstore.Execution, 0, len(runs))
	for _, run := range runs {
		execs = append(execs, run.Execution)
	}
	writeJSON(w, http.StatusOK, execs)
}

// getLastExecution restores the builder's per-node previews on page load, so
// reopening a project shows the last run's results without re-running it.
func (d *daemon) getLastExecution(w http.ResponseWriter, r *http.Request) {
	runs, err := d.store.ListRuns(r.PathValue("id"), 1)
	if err != nil || len(runs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"execution": nil, "nodes": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, executionResponse(&runs[0]))
}

// executionResponse is the frontend's ExecutionResponse: the run split back
// into the execution and its nodes. There is no queue locally, so the optional
// queue field is left off rather than faked.
func executionResponse(run *localstore.Run) map[string]any {
	return map[string]any{"execution": run.Execution, "nodes": run.Nodes}
}

// ---------- providers ----------

// providerInfo is the frontend's ProviderInfo, field for field. The hosted
// catalog lives in api/catalog.go behind unexported types and a per-user
// secret lookup in Mongo, so the local one is built here instead of reaching
// into a package that cannot answer without a database.
type providerInfo struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Purpose      string `json:"purpose"`
	KeyHint      string `json:"key_hint"`
	ConsoleURL   string `json:"console_url"`
	SetupHint    string `json:"setup_hint,omitempty"`
	PlatformKey  bool   `json:"platform_key"`
	UserKeyLast4 string `json:"user_key_last4"`
	HasUserKey   bool   `json:"has_user_key"`
	// Roles are the jobs this provider can do. A list, because a provider does
	// more than one: Gemini generates images and reads them, Ollama writes text
	// and reads images. It was a single role, which forced image and vision
	// under one heading and told somebody holding an Ollama key they needed a
	// Google one to read a screenshot.
	//
	// Derived from the engine's own sets rather than written out here: this
	// daemon *is* the engine, so a copy would be a copy of itself.
	Roles     []string `json:"roles"`
	IsDefault bool     `json:"is_default"`
	Required  bool     `json:"required"`
	// An endpoint provider is configured by an address and a model rather than
	// by a key, so the panel has to draw it differently: two fields and a model
	// list it can fetch, not a password box. The flag says which.
	Endpoint    bool   `json:"endpoint,omitempty"`
	EndpointURL string `json:"endpoint_url,omitempty"`
	DefaultURL  string `json:"default_url,omitempty"`
	Model       string `json:"model,omitempty"`
}

// localCatalog describes every provider the desktop app can use, in the
// frontend's ProviderInfo shape. It is rebuilt per request because the user can
// paste a key in the settings panel and expect the panel to update.
//
// The two flags keep their hosted meaning: has_user_key is "the user stored
// this one themselves" (secrets.json), platform_key is "it is already
// configured without them having to", which locally means the environment the
// daemon was launched with, or — for the CLI providers — the binary being
// installed and signed in.
func (d *daemon) localCatalog() []providerInfo {
	cfg := d.providerConfig()
	stored := map[string]string{}
	if v, err := d.store.SecretValues(); err == nil {
		stored = v
	}

	catalog := []providerInfo{
		{
			ID:         "google",
			Label:      "Google AI Studio",
			Purpose:    "Image generation, image editing, vision (Gemini) and video (Veo).",
			KeyHint:    "AIza…",
			ConsoleURL: "https://aistudio.google.com/apikey",
			SetupHint:  "Create an API key in Google AI Studio. Every image, vision and video node needs it. Video is billed by the second.",
		},
		{
			ID:         "bfl",
			Label:      "Black Forest Labs (FLUX)",
			Purpose:    "Image and video generation on FLUX models.",
			KeyHint:    "Black Forest Labs API key",
			ConsoleURL: "https://dashboard.bfl.ai/keys",
			SetupHint:  "Create a key in the Black Forest Labs dashboard. Images are billed once each; video is billed by the second, and the draft pass is a third of the price.",
		},
		{
			ID:         "anthropic",
			Label:      "Claude (Anthropic)",
			Purpose:    "Text generation and the Brain agent, on Claude models.",
			KeyHint:    "sk-ant-api… or a token from claude setup-token",
			ConsoleURL: "https://console.anthropic.com/settings/keys",
			SetupHint:  "Create an API key in the console, or run `claude setup-token` in your terminal and paste the token it prints.",
		},
		{
			ID:         "openai",
			Label:      "OpenAI (ChatGPT / Codex)",
			Purpose:    "Text generation and the Brain agent, on GPT models.",
			KeyHint:    "sk-…",
			ConsoleURL: "https://platform.openai.com/api-keys",
			SetupHint:  "Create an API key on the OpenAI platform. A ChatGPT or Codex subscription on its own does not include API access.",
		},
		{
			ID:         "ollama",
			Label:      "Ollama Cloud",
			Purpose:    "Text generation, vision and code completion, on open models.",
			KeyHint:    "Ollama API key",
			ConsoleURL: "https://ollama.com/settings/keys",
			SetupHint:  "Create a key in your Ollama account settings, or point OLLAMA_URL at an Ollama running on this machine.",
		},
		{
			ID:         "claude-cli",
			Label:      "Claude Code (local CLI)",
			Purpose:    "Text generation and the Brain agent through the claude binary already signed in on this machine.",
			ConsoleURL: "https://claude.com/claude-code",
			SetupHint:  "Install the Claude Code CLI so `claude` is on your PATH. Your subscription authenticates it; no key is stored here.",
		},
		{
			ID:         "codex-cli",
			Label:      "Codex (local CLI)",
			Purpose:    "Text generation and the Brain agent through the codex binary already signed in on this machine.",
			ConsoleURL: "https://developers.openai.com/codex/cli",
			SetupHint:  "Install the Codex CLI so `codex` is on your PATH. Your subscription authenticates it; no key is stored here.",
		},
		{
			ID:         "qwen-cli",
			Label:      "Qwen Code (local CLI)",
			Purpose:    "Text generation and the Brain agent through the qwen binary — the one harness that can be pointed at the models you already run here.",
			ConsoleURL: "https://github.com/QwenLM/qwen-code",
			SetupHint:  "Install Qwen Code so `qwen` is on your PATH. It runs on its own login by default, or against your Ollama or LM Studio through QWEN_CLI_ENDPOINT.",
		},
		{
			ID:         providers.OllamaLocalProvider,
			Label:      "Ollama (this machine)",
			Purpose:    "Text generation, vision and code completion, on the models you have pulled locally.",
			ConsoleURL: "https://ollama.com/download",
			SetupHint:  "Run Ollama on this machine, then pick a model. Nothing is sent anywhere — which is the only way to ask about an image without the image leaving your computer.",
		},
		{
			ID:         providers.LMStudioProvider,
			Label:      "LM Studio (this machine)",
			Purpose:    "Text generation, vision and code completion, on the models loaded in LM Studio.",
			ConsoleURL: "https://lmstudio.ai",
			SetupHint:  "Start LM Studio's local server, then pick a model. It speaks the same API as OpenAI, so everything here works the same way.",
		},
		{
			ID:        providers.CustomProvider,
			Label:     "Custom endpoint",
			Purpose:   "Text generation, vision and code completion on any server speaking the OpenAI API — llama.cpp, vLLM, LocalAI, a box on your network.",
			KeyHint:   "http://host:port/v1",
			SetupHint: "Give the base address, including /v1. A key only if that server asks for one.",
		},
		{
			ID:        providers.CustomImageProvider,
			Label:     "Custom image endpoint",
			Purpose:   "Image generation and editing on any server speaking the OpenAI images API — a diffusion model on this machine, or one on your network.",
			KeyHint:   "http://host:port/v1",
			SetupHint: "Give the base address, including /v1. The server needs /v1/images/generations, and /v1/images/edits if you want to edit with reference images.",
		},
	}

	// Which jobs are covered by something the run can actually use.
	covered := map[string]bool{}
	for i := range catalog {
		id := catalog[i].ID
		eff := effectiveCredential(cfg, id)
		if isEndpointProvider(id) {
			// There is no key to show four characters of: having an address is
			// the whole of being configured. The address and model come back so
			// the panel can show what this project is pointed at.
			e := cfg.EndpointFor(id)
			catalog[i].Endpoint = true
			catalog[i].EndpointURL = e.URL
			catalog[i].DefaultURL = providers.DefaultEndpointURL(id)
			catalog[i].Model = e.Model
			catalog[i].HasUserKey = eff != ""
		} else if isCLIProvider(id) {
			// A CLI provider has no key to store: being installed and signed in
			// is the whole of its configuration, so there is nothing to show
			// the last four characters of.
			catalog[i].HasUserKey = eff != ""
		} else {
			catalog[i].HasUserKey = stored[id] != ""
			catalog[i].UserKeyLast4 = last4(stored[id])
			catalog[i].PlatformKey = eff != "" && stored[id] == ""
		}
		catalog[i].Roles = localRolesOf(id)
		if eff != "" {
			for _, role := range catalog[i].Roles {
				covered[role] = true
			}
		}
	}
	for i := range catalog {
		catalog[i].IsDefault = hasRole(catalog[i].Roles, "text") && catalog[i].ID == cfg.TextProvider
		// Providers of one job are alternatives, so holding any of them
		// satisfies that job. A provider is still needed when one of the jobs
		// it could do is covered by nothing else — which is why this asks about
		// each of its jobs rather than about the provider.
		catalog[i].Required = false
		for _, role := range catalog[i].Roles {
			if !covered[role] {
				catalog[i].Required = true
			}
		}
	}
	return catalog
}

// localRolesOf asks the engine which jobs a provider can do. The CLI providers
// are text only: they drive a command line tool that writes, and neither of
// them takes an image.
func localRolesOf(id string) []string {
	var roles []string
	for _, role := range []struct {
		name string
		list []string
	}{
		{"text", providers.TextProviders},
		{"image", providers.ImageProviders},
		{"vision", providers.VisionProviders},
		{"video", providers.VideoProviders},
		{"completion", providers.CompletionProviders},
	} {
		for _, candidate := range role.list {
			if candidate == id {
				roles = append(roles, role.name)
				break
			}
		}
	}
	return roles
}

func hasRole(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// effectiveCredential is what a provider would actually run with, after
// secrets.json and the environment have both been folded in. An empty string
// means the provider is not usable.
func effectiveCredential(cfg *providers.Config, id string) string {
	switch id {
	case "google":
		return cfg.GoogleAPIKey
	case "bfl":
		return cfg.BFLAPIKey
	case "anthropic":
		return cfg.AnthropicAPIKey
	case "openai":
		return cfg.OpenAIAPIKey
	case "ollama":
		return cfg.OllamaAPIKey
	case "claude-cli", "codex-cli", "qwen-cli":
		// TextCredential resolves the CLI to its binary path, and returns ""
		// when it is not installed, which is exactly "not configured" here.
		return cfg.TextCredential(id)
	}
	if isEndpointProvider(id) {
		// An address is the credential. Ollama on this machine and LM Studio
		// are configured the moment they are running, which is why they count
		// as usable without anybody pasting anything.
		return cfg.EndpointFor(id).URL
	}
	return ""
}

// isCLIProvider asks the engine, which is the same binary: a list of these here
// would be a copy of a list one package away.
func isCLIProvider(id string) bool { return providers.IsCLIProvider(id) }

// isEndpointProvider says whether a provider is configured by an address rather
// than by a key. Asked of the engine, not listed here, so adding a fourth one
// there does not need a matching edit in this file.
func isEndpointProvider(id string) bool {
	for _, p := range providers.AddressConfiguredProviders {
		if p == id {
			return true
		}
	}
	return false
}

// last4 is how much of a credential is safe to show: enough to recognize which
// key is configured, not enough to be worth leaking.
func last4(secret string) string {
	if len(secret) < 4 {
		return ""
	}
	return secret[len(secret)-4:]
}

func (d *daemon) listProviders(w http.ResponseWriter, r *http.Request) {
	// The project's own order travels with the catalogue, so the panel can show
	// the list the way runs will actually use it rather than in whatever order
	// this file happens to declare them.
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": d.localCatalog(),
		"order":     d.store.ProviderOrder(),
	})
}

// setProviderOrder records which provider this project wants tried first for a
// job. Les métiers connus sont ceux que le moteur nomme — une liste écrite ici
// aurait oublié la complétion le jour où elle est arrivée.
//
// Only ids this engine can actually drive, for a job they can actually do:
// a list naming Black Forest Labs for vision would send every such call to a
// backend that cannot answer, and nothing would say why.
func (d *daemon) setProviderOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Order map[string][]string `json:"order"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	known := map[string][]string{}
	for _, p := range d.localCatalog() {
		known[p.ID] = p.Roles
	}
	clean := map[string][]string{}
	for role, ids := range req.Order {
		if len(providers.ProvidersFor(role)) == 0 {
			continue
		}
		seen := map[string]bool{}
		var kept []string
		for _, id := range ids {
			roles, ok := known[id]
			if !ok || seen[id] || !hasRole(roles, role) {
				continue
			}
			seen[id] = true
			kept = append(kept, id)
		}
		if len(kept) > 0 {
			clean[role] = kept
		}
	}

	if err := d.store.SetProviderOrder(clean); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the order")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": clean})
}

// setProviderEndpoint records where one local server is and which model it
// answers with by default.
//
// Clearing both fields is how one of these is turned off: they have no key to
// delete, so "forget this provider" has to be spelled some other way, and an
// empty address is the same statement the empty key box makes elsewhere.
func (d *daemon) setProviderEndpoint(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isEndpointProvider(id) {
		writeErr(w, http.StatusBadRequest, "that provider is not configured by an address")
		return
	}
	// Des pointeurs, et c'est tout l'intérêt : un champ absent n'est pas un
	// champ vide.
	//
	// Le panneau enregistre une adresse sans toujours renvoyer le modèle — il
	// ne l'a pas encore chargé, ou le catalogue l'omet parce qu'il est vide —
	// et la version d'avant remplaçait l'entrée entière. Résultat : réenregistrer
	// une adresse effaçait le modèle choisi, en silence, et la complétion
	// suivante répondait « aucun modèle choisi » pour un réglage qu'on venait
	// de voir à l'écran.
	var req struct {
		URL   *string `json:"url"`
		Key   *string `json:"key"`
		Model *string `json:"model"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	current := d.store.ProviderEndpoints()[id]
	url := strings.TrimSpace(current.URL)
	if req.URL != nil {
		url = strings.TrimSpace(*req.URL)
	}
	if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		// Said here rather than at the first call: a bare host:port is the
		// likeliest thing to type, and letting it through produces a transport
		// error whose text is about a missing scheme rather than about the box
		// that needs one.
		writeErr(w, http.StatusBadRequest, "the address needs to start with http:// or https://")
		return
	}
	key := current.Key
	if req.Key != nil {
		key = strings.TrimSpace(*req.Key)
	}
	model := current.Model
	if req.Model != nil {
		model = strings.TrimSpace(*req.Model)
	}
	if err := d.store.SetProviderEndpoint(id, localstore.Endpoint{
		URL:   url,
		Key:   key,
		Model: model,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the endpoint")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": d.localCatalog()})
}

// providerModels asks a local server what it can run.
//
// Asked of the server rather than listed anywhere: what is installed is the
// person's business and changes whenever they pull something new, so any list
// we shipped would be wrong by the end of the week.
func (d *daemon) providerModels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isEndpointProvider(id) {
		writeErr(w, http.StatusBadRequest, "that provider has no model list")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := d.providerConfig().ModelList(ctx, id)
	if err != nil {
		// The server's own words: "connection refused" means it is not running,
		// and that is the answer somebody needs, not a generic failure.
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// knownProvider guards the secrets file against entries no run would ever read.
func (d *daemon) knownProvider(id string) bool {
	for _, p := range d.localCatalog() {
		if p.ID == id {
			return true
		}
	}
	return false
}

// ---------- secrets ----------

func (d *daemon) listSecrets(w http.ResponseWriter, r *http.Request) {
	secrets, err := d.store.ListSecrets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read secrets")
		return
	}
	writeJSON(w, http.StatusOK, secrets)
}

func (d *daemon) setSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Secret   string `json:"secret"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if !d.knownProvider(provider) {
		writeErr(w, http.StatusBadRequest, "unknown provider")
		return
	}
	if isCLIProvider(provider) {
		writeErr(w, http.StatusBadRequest, "the local CLI providers are configured by installing them, not by storing a key")
		return
	}
	secret, err := d.store.SetSecret(provider, req.Secret)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The response never echoes the secret back, only enough to identify it.
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":     secret.Provider,
		"secret_last4": secret.SecretLast4,
	})
}

func (d *daemon) deleteSecret(w http.ResponseWriter, r *http.Request) {
	provider := strings.ToLower(strings.TrimSpace(r.PathValue("provider")))
	if err := d.store.DeleteSecret(provider); err != nil {
		writeErr(w, http.StatusBadRequest, "unknown provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// aiQuota reports that the free writing helper is unavailable. Hosted, it is a
// courtesy funded by the platform's own key; there is no platform key on a
// laptop, so rather than spend the user's credits on a convenience they did not
// ask for, the feature is simply off and the panel hides the button.
func (d *daemon) aiQuota(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"allowed":             false,
		"retry_after_seconds": 0,
		"window_seconds":      0,
	})
}

// codeCompletion remplit le milieu d'un fichier, pour l'éditeur.
//
// Une requête par frappe au repos, donc tout ici est taillé pour être court :
// pas de trace dans le journal, pas d'exécution enregistrée, et une réponse
// vide plutôt qu'une erreur quand il n'y a rien à proposer. Une complétion qui
// n'aboutit pas ne doit rien coûter à lire.
//
// L'annulation vient du contexte de la requête : quand l'éditeur abandonne —
// et il abandonne à chaque touche — la connexion se ferme, et l'appel au modèle
// s'arrête avec elle.
func (d *daemon) codeCompletion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prefix    string `json:"prefix"`
		Suffix    string `json:"suffix"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	// Un plafond de caractères plutôt qu'un plafond de jetons : le contexte
	// utile est ce qui entoure le curseur, et envoyer un fichier de dix mille
	// lignes à chaque frappe coûterait le temps qu'il met à voyager.
	const window = 4000
	prefix, suffix := req.Prefix, req.Suffix
	if len(prefix) > window {
		prefix = prefix[len(prefix)-window:]
	}
	if len(suffix) > window {
		suffix = suffix[:window]
	}

	text, err := d.providerConfig().CodeCompletion(r.Context(), providers.CompletionRequest{
		Provider:  req.Provider,
		Model:     req.Model,
		Prefix:    prefix,
		Suffix:    suffix,
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		if errors.Is(r.Context().Err(), context.Canceled) {
			// L'éditeur est passé à autre chose. Ce n'est pas un échec, et le
			// dire en erreur ferait clignoter un message à chaque frappe.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text})
}

// localStatus backs the desktop status bar: which project is open, how much is
// in it, and what it is currently able to run.
func (d *daemon) localStatus(w http.ResponseWriter, r *http.Request) {
	wfs, err := d.store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read project")
		return
	}
	cfg := d.providerConfig()
	configured := map[string]bool{}
	for _, p := range d.localCatalog() {
		configured[p.ID] = effectiveCredential(cfg, p.ID) != ""
	}
	packs, failedPacks := d.packSummary()
	writeJSON(w, http.StatusOK, map[string]any{
		// The running engine's own version, so the desktop can compare it
		// against the release channel without spawning a second process just
		// to read --version off the binary it already started.
		"version":   Version,
		"commit":    Commit,
		"project":   d.store.Root,
		"workflows": len(wfs),
		// The packs this project carries. A workflow that only runs here runs
		// here because of these, so which ones loaded belongs next to which
		// providers are configured — and so does how many did not, because a
		// node missing from the palette otherwise has no explanation anywhere
		// the user is looking.
		"packs":        len(packs),
		"pack_names":   packs,
		"packs_failed": failedPacks,
		"providers":    configured,
		"cli": map[string]bool{
			"claude": cfg.TextCredential("claude-cli") != "",
			"codex":  cfg.TextCredential("codex-cli") != "",
			"qwen":   cfg.TextCredential("qwen-cli") != "",
		},
		// The node types that only work here. The builder is shared with the
		// hosted app, so it asks the backend it is talking to what it can run
		// rather than deciding from how it was launched.
		"local_nodes": engine.LocalOnlyNodeTypes,
	})
}

// ---------- media ----------

// serveMedia hands back files from .zyvro/media. This route carries no token
// (a browser cannot authenticate an <img src>), so the check below is the only
// thing standing between a crafted path and the user's disk: the path is
// resolved and then required to still be inside the media directory, because a
// "../" that escaped here would serve any file on the machine to anything that
// can reach the port.
func (d *daemon) serveMedia(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	root, err := filepath.Abs(d.store.MediaDir())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, full)
}

// ---------- small helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// writeStoreErr collapses a rejected id and a missing file into the same 404.
// Telling them apart would let a caller use the API to probe what exists.
func writeStoreErr(w http.ResponseWriter, err error, msg string) {
	if errors.Is(err, localstore.ErrNotFound) || errors.Is(err, localstore.ErrInvalidID) {
		writeErr(w, http.StatusNotFound, msg)
		return
	}
	writeErr(w, http.StatusInternalServerError, msg)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// sanitizeOutput drops embedded base64 image payloads, keeping the URLs that
// point at the media directory. Without it every run file would carry a few
// megabytes of base64 into a directory the user is expected to commit, and
// every poll would pull it back out again.
func sanitizeOutput(s string) string {
	if s == "" {
		return s
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	sanitizeValue(v)
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

func sanitizeValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for key, val := range t {
			if key == "dataUrl" {
				if s, ok := val.(string); ok && len(s) > 512 {
					t[key] = "truncated-data-url"
				}
				continue
			}
			sanitizeValue(val)
		}
	case []any:
		for _, item := range t {
			sanitizeValue(item)
		}
	}
}
