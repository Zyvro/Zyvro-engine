package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MiMo Code, read through what the installed binary actually printed: the
// success stream below and the error line are copied from `mimo run --format
// json`, version 0.1.15.

const mimoSuccess = `{"type":"step_start","timestamp":1791004000641,"sessionID":"ses_1","part":{"id":"prt_1","messageID":"msg_1","sessionID":"ses_1","type":"step-start"}}
{"type":"text","timestamp":1791004001308,"sessionID":"ses_1","part":{"id":"prt_2","messageID":"msg_1","sessionID":"ses_1","type":"text","text":"OK","time":{"start":1,"end":2}}}
{"type":"step_finish","timestamp":1791004001309,"sessionID":"ses_1","part":{"id":"prt_3","reason":"stop","messageID":"msg_1","sessionID":"ses_1","type":"step-finish","tokens":{"total":25608,"input":25587,"output":3,"reasoning":18,"cache":{"write":0,"read":0}},"cost":0.00358806}}`

const mimoModelMissing = `{"type":"error","timestamp":1791004003266,"sessionID":"ses_2","error":{"name":"UnknownError","data":{"message":"Model not found: xiaomi/does-not-exist."}}}`

func mimoCfg(t *testing.T, body string) *Config {
	t.Helper()
	return &Config{MimoCLIPath: fakeCLI(t, body), LocalCLIWorkdir: t.TempDir()}
}

func TestMimoCLIReadsTheAnswer(t *testing.T) {
	c := mimoCfg(t, "cat >/dev/null\ncat <<'EOF'\n"+mimoSuccess+"\nEOF")
	got, err := c.LLMComplete(context.Background(), LLMRequest{Provider: mimoCLIProvider, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "OK" {
		t.Fatalf("content = %q", got.Content)
	}
}

func TestMimoCLIErrorWithExitZeroIsAnError(t *testing.T) {
	// The binary exits 0 on a model it does not have; only the stream says so.
	c := mimoCfg(t, "cat >/dev/null\necho '"+mimoModelMissing+"'\nexit 0")
	_, err := c.LLMComplete(context.Background(), LLMRequest{Provider: mimoCLIProvider, Messages: []Message{{Role: "user", Content: "hi"}}})
	pe := providerErr(t, err)
	if !strings.Contains(pe.Message, "Model not found: xiaomi/does-not-exist.") {
		t.Fatalf("message = %q", pe.Message)
	}
}

func TestMimoCLIKeepsTheLastMessage(t *testing.T) {
	stream := `{"type":"text","part":{"messageID":"m1","text":"Let me look."}}
{"type":"text","part":{"messageID":"m2","text":"The answer "}}
{"type":"text","part":{"messageID":"m2","text":"is 4."}}`
	answer, failure, parsed := parseMimoCLI(stream)
	if !parsed || failure != "" || answer != "The answer is 4." {
		t.Fatalf("answer=%q failure=%q parsed=%v", answer, failure, parsed)
	}
}

func TestMimoCLIRunsReadOnlyWithTheModelAndPromptOnStdin(t *testing.T) {
	dir := t.TempDir()
	argsFile, stdinFile := filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	c := mimoCfg(t, `echo "$@" > `+argsFile+`
cat > `+stdinFile+`
echo '{"type":"text","part":{"messageID":"m","text":"ok"}}'`)
	_, err := c.LLMComplete(context.Background(), LLMRequest{
		Provider: mimoCLIProvider,
		Model:    "xiaomi/mimo-v2.6-pro",
		Messages: []Message{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "secret question"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if got := strings.TrimSpace(string(args)); got != "run --format json --agent plan --model xiaomi/mimo-v2.6-pro" {
		t.Fatalf("args = %q", got)
	}
	stdin, _ := os.ReadFile(stdinFile)
	if !strings.HasPrefix(string(stdin), "Be brief.") || !strings.Contains(string(stdin), "secret question") {
		t.Fatalf("stdin = %q", stdin)
	}
	if strings.Contains(string(args), "secret") {
		t.Fatal("the prompt reached the command line")
	}
}

func TestMimoCLIIsALocalCLIProvider(t *testing.T) {
	if !IsCLIProvider(mimoCLIProvider) {
		t.Fatal("mimo-cli is not a CLI provider")
	}
	for _, p := range HostedTextProviders() {
		if p == mimoCLIProvider {
			t.Fatal("mimo-cli offered on the hosted service")
		}
	}
	_, install := (&Config{}).localCLIBinary(mimoCLIProvider)
	if install != "curl -fsSL https://mimo.xiaomi.com/install | bash" {
		t.Fatalf("install = %q", install)
	}
}

func TestMimoCLIIsFoundWhereItsInstallerPutsIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	bin := filepath.Join(home, ".mimocode", "bin", "mimo")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := (&Config{MimoCLIPath: "mimo"}).localCLIBinary(mimoCLIProvider); got != bin {
		t.Fatalf("binary = %q, want %q", got, bin)
	}
}
