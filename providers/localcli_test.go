package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The local CLI providers shell out to a binary the user installed themselves,
// which is exactly what a test must not do: a real run would spend the
// developer's own subscription and would answer differently every time. So
// every test here points the configured path at a tiny shell script that prints
// a recorded CLI output. That pins the parsing, the flags and the stdin
// contract without any CLI being installed.

// fakeCLI writes an executable script and returns its path.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("could not write the fake CLI: %v", err)
	}
	return path
}

func claudeCfg(t *testing.T, body string) *Config {
	t.Helper()
	return &Config{ClaudeCLIPath: fakeCLI(t, body), LocalCLIWorkdir: t.TempDir()}
}

func codexCfg(t *testing.T, body string) *Config {
	t.Helper()
	return &Config{CodexCLIPath: fakeCLI(t, body), LocalCLIWorkdir: t.TempDir()}
}

func userTurn(text string) LLMRequest {
	return LLMRequest{Messages: []Message{{Role: "user", Content: text}}}
}

// providerErr unwraps the error the node layer will branch on.
func providerErr(t *testing.T, err error) *ProviderError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a *ProviderError, got %T: %v", err, err)
	}
	return pe
}

// The happy path for each CLI, plus the shapes each one is allowed to return.
func TestLocalCLIParsesOutput(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		script string
		want   string
	}{
		{
			name:   "claude returns the result field of a success envelope",
			kind:   claudeCLIProvider,
			script: `echo '{"type":"result","subtype":"success","is_error":false,"duration_ms":812,"result":"hello","session_id":"abc"}'`,
			want:   "hello",
		},
		{
			name:   "claude output that is not JSON is still an answer",
			kind:   claudeCLIProvider,
			script: `echo 'just plain text'`,
			want:   "just plain text",
		},
		{
			name: "codex takes the last agent message of the JSONL stream",
			kind: codexCLIProvider,
			script: `echo '{"type":"thread.started","thread_id":"t1"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"thinking out loud"}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"final answer"}}'`,
			want: "final answer",
		},
		{
			name:   "codex also understands the msg.agent_message shape",
			kind:   codexCLIProvider,
			script: `echo '{"id":"0","msg":{"type":"agent_message","message":"alternate shape"}}'`,
			want:   "alternate shape",
		},
		{
			name:   "codex also understands a top level last_agent_message",
			kind:   codexCLIProvider,
			script: `echo '{"type":"turn.completed","last_agent_message":"wrapped up"}'`,
			want:   "wrapped up",
		},
		{
			name: "codex ignores noise around the events it understands",
			kind: codexCLIProvider,
			script: `echo 'not json at all'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"ignored because it parses"}}'`,
			want: "ignored because it parses",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *Config
			if tc.kind == codexCLIProvider {
				cfg = codexCfg(t, tc.script)
			} else {
				cfg = claudeCfg(t, tc.script)
			}
			resp, err := cfg.localCLIComplete(context.Background(), userTurn("hi"), tc.kind)
			if err != nil {
				t.Fatalf("call failed: %v", err)
			}
			if resp.Content != tc.want {
				t.Fatalf("content = %q, want %q", resp.Content, tc.want)
			}
		})
	}
}

// When no event parses at all, the raw text is the only answer available.
func TestCodexCLIFallsBackToRawOutput(t *testing.T) {
	cfg := codexCfg(t, `echo 'the CLI printed prose'`)
	resp, err := cfg.localCLIComplete(context.Background(), userTurn("hi"), codexCLIProvider)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if resp.Content != "the CLI printed prose" {
		t.Fatalf("content = %q", resp.Content)
	}
}

// is_error reuses the result field for the reason, so a success-shaped envelope
// can still be a failure. Returning it as content would show the user an error
// message as if the model had written it.
func TestClaudeCLISurfacesAnErrorEnvelope(t *testing.T) {
	cfg := claudeCfg(t, `echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Credit balance is too low"}'`)

	_, err := cfg.localCLIComplete(context.Background(), userTurn("hi"), claudeCLIProvider)
	pe := providerErr(t, err)
	if pe.Code != "cli_error" {
		t.Fatalf("code = %q, want cli_error", pe.Code)
	}
	if !strings.Contains(pe.Message, "Credit balance") {
		t.Fatalf("the reason was lost: %q", pe.Message)
	}
}

// A non-zero exit is the only place the CLI explains itself, so its stderr has
// to reach the node error.
func TestLocalCLIReportsANonZeroExit(t *testing.T) {
	cfg := claudeCfg(t, `echo 'usage: claude [options]' >&2
exit 2`)

	_, err := cfg.localCLIComplete(context.Background(), userTurn("hi"), claudeCLIProvider)
	pe := providerErr(t, err)
	if pe.Code != "cli_error" {
		t.Fatalf("code = %q, want cli_error", pe.Code)
	}
	if !strings.Contains(pe.Message, "usage: claude") {
		t.Fatalf("stderr did not reach the error: %q", pe.Message)
	}
}

// "Not installed" is a different problem from "the run failed": the user has to
// install something, so it gets its own code and names the binary.
func TestLocalCLIReportsAMissingBinary(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"a bare name that is not on PATH", "zyvro-no-such-cli"},
		{"a configured path that does not exist", filepath.Join(t.TempDir(), "zyvro-no-such-cli")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{ClaudeCLIPath: tc.path}
			_, err := cfg.localCLIComplete(context.Background(), userTurn("hi"), claudeCLIProvider)
			pe := providerErr(t, err)
			if pe.Code != "not_installed" {
				t.Fatalf("code = %q, want not_installed", pe.Code)
			}
			if !strings.Contains(pe.Message, "zyvro-no-such-cli") || !strings.Contains(pe.Message, "npm install") {
				t.Fatalf("the message must name the binary and how to install it: %q", pe.Message)
			}
		})
	}
}

// A CLI cannot be handed an OpenAI tool schema. Dropping the tools quietly
// would leave the Brain waiting for calls that can never come.
func TestLocalCLIRefusesToolCallingNodes(t *testing.T) {
	for _, kind := range []string{claudeCLIProvider, codexCLIProvider} {
		t.Run(kind, func(t *testing.T) {
			// The script would succeed; the refusal must happen before it runs.
			cfg := &Config{
				ClaudeCLIPath: fakeCLI(t, `echo '{"type":"result","subtype":"success","result":"hi"}'`),
				CodexCLIPath:  fakeCLI(t, `echo '{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}'`),
			}
			req := userTurn("hi")
			req.Tools = []Tool{{Type: "function", Function: ToolSchema{Name: "get_weather"}}}

			_, err := cfg.localCLIComplete(context.Background(), req, kind)
			pe := providerErr(t, err)
			if pe.Code != "unsupported" {
				t.Fatalf("code = %q, want unsupported", pe.Code)
			}
			if !strings.Contains(pe.Message, "Brain") {
				t.Fatalf("the message should point at the fix: %q", pe.Message)
			}
		})
	}
}

// The prompt goes on stdin, never as an argument: it can hold user data, and an
// argument is visible to anyone who can list processes.
func TestClaudeCLIReceivesThePromptOnStdin(t *testing.T) {
	cfg := claudeCfg(t, `printf '{"type":"result","subtype":"success","result":"%s"}\n' "$(cat)"`)

	resp, err := cfg.localCLIComplete(context.Background(), LLMRequest{
		Messages: []Message{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: "first answer"},
			{Role: "user", Content: "second question"},
		},
	}, claudeCLIProvider)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	for _, want := range []string{"user: first question", "assistant: first answer", "user: second question"} {
		if !strings.Contains(resp.Content, want) {
			t.Fatalf("the CLI did not receive %q on stdin; it read %q", want, resp.Content)
		}
	}
}

// Codex has no system-prompt flag, so the system block has to lead the prompt
// it reads from stdin.
func TestCodexCLIPutsTheSystemBlockFirstOnStdin(t *testing.T) {
	cfg := codexCfg(t, `cat`)

	resp, err := cfg.localCLIComplete(context.Background(), LLMRequest{
		Messages: []Message{
			{Role: "system", Content: "Be terse."},
			{Role: "user", Content: "capital of France?"},
		},
	}, codexCLIProvider)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if !strings.HasPrefix(resp.Content, "Be terse.") {
		t.Fatalf("the system block must lead the prompt, got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "user: capital of France?") {
		t.Fatalf("the turn was lost: %q", resp.Content)
	}
}

// Claude takes its system prompt as a flag, and a configured model must reach
// the command line.
func TestClaudeCLIPassesModelAndSystemFlags(t *testing.T) {
	cfg := claudeCfg(t, `printf '{"type":"result","subtype":"success","result":"%s"}\n' "$*"`)
	cfg.ClaudeCLIModel = "opus"

	resp, err := cfg.localCLIComplete(context.Background(), LLMRequest{
		Messages: []Message{
			{Role: "system", Content: "Be terse."},
			{Role: "user", Content: "hi"},
		},
	}, claudeCLIProvider)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	for _, want := range []string{"-p", "--output-format json", "--model opus", "--append-system-prompt Be terse."} {
		if !strings.Contains(resp.Content, want) {
			t.Fatalf("missing %q in the command line %q", want, resp.Content)
		}
	}
}

// An unset model is the correct default: the CLI then uses whatever the
// subscription already picked, and no flag is sent at all.
func TestClaudeCLIOmitsTheModelFlagWhenUnset(t *testing.T) {
	cfg := claudeCfg(t, `printf '{"type":"result","subtype":"success","result":"%s"}\n' "$*"`)

	resp, err := cfg.localCLIComplete(context.Background(), userTurn("hi"), claudeCLIProvider)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if strings.Contains(resp.Content, "--model") {
		t.Fatalf("no model was configured, so no --model flag should be sent: %q", resp.Content)
	}
}

func TestFlattenMessages(t *testing.T) {
	cases := []struct {
		name       string
		msgs       []Message
		wantSystem string
		wantPrompt string
	}{
		{
			name: "system turns are collected and the rest keeps its order",
			msgs: []Message{
				{Role: "system", Content: "Be terse."},
				{Role: "user", Content: "one"},
				{Role: "assistant", Content: "two"},
				{Role: "system", Content: "Answer in French."},
				{Role: "user", Content: "three"},
			},
			wantSystem: "Be terse.\n\nAnswer in French.",
			wantPrompt: "user: one\n\nassistant: two\n\nuser: three",
		},
		{
			name:       "empty turns are dropped",
			msgs:       []Message{{Role: "user", Content: "  "}, {Role: "user", Content: "real"}},
			wantSystem: "",
			wantPrompt: "user: real",
		},
		{
			name:       "a turn without a role is treated as the user speaking",
			msgs:       []Message{{Content: "hello"}},
			wantSystem: "",
			wantPrompt: "user: hello",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			system, prompt := flattenMessages(tc.msgs)
			if system != tc.wantSystem {
				t.Fatalf("system = %q, want %q", system, tc.wantSystem)
			}
			if prompt != tc.wantPrompt {
				t.Fatalf("prompt = %q, want %q", prompt, tc.wantPrompt)
			}
		})
	}
}

// The router is what the rest of the codebase calls, and a CLI provider must
// never be mistaken for one with a credential.
func TestLocalCLIRouting(t *testing.T) {
	script := fakeCLI(t, `echo '{"type":"result","subtype":"success","result":"routed"}'`)
	cfg := &Config{
		TextProvider:   "ollama",
		ClaudeCLIPath:  script,
		CodexCLIPath:   "zyvro-no-such-cli",
		ClaudeCLIModel: "opus",
	}

	if got := cfg.resolveTextProvider("claude-cli"); got != claudeCLIProvider {
		t.Fatalf("resolveTextProvider(claude-cli) = %q", got)
	}
	if got := cfg.resolveTextProvider("codex-cli"); got != codexCLIProvider {
		t.Fatalf("resolveTextProvider(codex-cli) = %q", got)
	}
	if got := cfg.TextModel("claude-cli"); got != "opus" {
		t.Fatalf("TextModel(claude-cli) = %q", got)
	}
	if got := cfg.TextModel("codex-cli"); got != "" {
		t.Fatalf("an unset CLI model must stay empty, got %q", got)
	}

	// The credential is only ever tested for emptiness, and must never be a
	// secret: an installed CLI reports its path, a missing one reports nothing.
	if got := cfg.TextCredential("claude-cli"); got != script {
		t.Fatalf("TextCredential(claude-cli) = %q, want the resolved binary path", got)
	}
	if got := cfg.TextCredential("codex-cli"); got != "" {
		t.Fatalf("a missing CLI must report no credential, got %q", got)
	}

	if !cfg.LocalCLIAvailable("claude-cli") {
		t.Fatal("the fake claude CLI exists, so it must report available")
	}
	if cfg.LocalCLIAvailable("codex-cli") {
		t.Fatal("a binary that is not on PATH must not report available")
	}
	// Asking about a non-CLI provider must not fall through to the default.
	if cfg.LocalCLIAvailable("anthropic") {
		t.Fatal("anthropic is not a local CLI provider")
	}

	resp, err := cfg.LLMComplete(context.Background(), LLMRequest{
		Provider: "claude-cli",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("LLMComplete failed: %v", err)
	}
	if resp.Content != "routed" {
		t.Fatalf("content = %q", resp.Content)
	}
}

// Codex reports why a run failed as an event on stdout and still exits
// non-zero. These cases are a regression guard: the first implementation
// returned on the exit status alone, so a real failure ("upgrade your CLI")
// reached the user as the word "exit status 1" and nothing else.
func TestCodexCLISurfacesErrorEventsFromStdout(t *testing.T) {
	apiError := `{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The model requires a newer version of Codex.\"}}`

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "a turn.failed event carrying a quoted API error",
			body: `echo '{"type":"thread.started","thread_id":"t1"}'
echo '{"type":"turn.failed","error":{"message":"` + apiError + `"}}'
exit 1`,
			want: "The model requires a newer version of Codex.",
		},
		{
			name: "a bare error event",
			body: `echo '{"type":"error","message":"sandbox denied write access"}'
exit 1`,
			want: "sandbox denied write access",
		},
		{
			name: "an error item inside item.completed",
			body: `echo '{"type":"item.completed","item":{"id":"i0","type":"error","message":"model metadata not found"}}'
exit 1`,
			want: "model metadata not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := codexCfg(t, tc.body).localCLIComplete(context.Background(), userTurn("hi"), codexCLIProvider)
			pe := providerErr(t, err)
			if pe.Code != "cli_error" {
				t.Fatalf("code = %q, want cli_error", pe.Code)
			}
			if !strings.Contains(pe.Message, tc.want) {
				t.Fatalf("message = %q, want it to contain %q", pe.Message, tc.want)
			}
		})
	}
}

// A successful run must not be derailed by an advisory error item earlier in
// the stream: Codex emits one for a metadata warning and then answers anyway.
func TestCodexCLIKeepsTheAnswerAfterAnAdvisoryErrorItem(t *testing.T) {
	body := `echo '{"type":"item.completed","item":{"id":"i0","type":"error","message":"metadata fallback"}}'
echo '{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"ORANGE"}}'`
	resp, err := codexCfg(t, body).localCLIComplete(context.Background(), userTurn("colour?"), codexCLIProvider)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "ORANGE" {
		t.Fatalf("content = %q, want ORANGE", resp.Content)
	}
}

// Codex refuses to start in a folder that is not a trusted git repository, and
// a project folder frequently is not one. The flag that waives that check is
// load-bearing, so its presence is asserted rather than assumed.
func TestCodexCLISkipsTheGitRepoCheck(t *testing.T) {
	body := `printf '%s\n' "$@" > "$ZYVRO_ARGS_FILE"
echo '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}'`
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("ZYVRO_ARGS_FILE", argsFile)

	if _, err := codexCfg(t, body).localCLIComplete(context.Background(), userTurn("hi"), codexCLIProvider); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the fake CLI did not record its arguments: %v", err)
	}
	if !strings.Contains(string(recorded), "--skip-git-repo-check") {
		t.Fatalf("arguments = %q, want them to include --skip-git-repo-check", recorded)
	}
}
