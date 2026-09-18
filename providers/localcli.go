package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Local CLI text providers. Almost everyone who pays for ChatGPT or Claude pays
// for a *subscription*, and a subscription cannot be spent through an API call
// from a hosted server. What it can do is drive the agent CLI the user already
// installed and signed in on their own machine, so the desktop app runs a text
// node by shelling out to that binary. No credential is read, stored or
// forwarded here: the CLI owns the session and Zyvro only sees the answer.
//
// Because the process runs as the user, these providers are only ever selected
// when the engine runs locally. The hosted server must keep using an API
// provider.

const (
	claudeCLIProvider = "claude-cli"
	codexCLIProvider  = "codex-cli"
	qwenCLIProvider   = "qwen-cli"

	// defaultLocalCLITimeout bounds one call. These CLIs are agents rather than
	// a single completion, so a long run is normal and the cap is generous; it
	// exists to stop a hung subprocess from wedging an execution forever.
	defaultLocalCLITimeout = 10 * time.Minute

	// localCLIStderrCap keeps a failing CLI's diagnostics readable in the node
	// error without pasting a whole stack trace into the workflow log.
	localCLIStderrCap = 500
)

// localCLIKind normalizes a provider name to one of the two CLI providers, or
// returns empty when the caller named something else. It deliberately does not
// fall back to the deployment default: asking "is the Claude CLI usable" must
// never accidentally answer for Ollama.
func localCLIKind(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case claudeCLIProvider:
		return claudeCLIProvider
	case codexCLIProvider:
		return codexCLIProvider
	case qwenCLIProvider:
		return qwenCLIProvider
	}
	return ""
}

// localCLIBinary resolves the binary to run and the command that installs it,
// so a missing CLI can tell the user how to fix it themselves.
func (c *Config) localCLIBinary(kind string) (bin, install string) {
	if kind == codexCLIProvider {
		bin = strings.TrimSpace(c.CodexCLIPath)
		if bin == "" {
			bin = "codex"
		}
		return bin, "npm install -g @openai/codex"
	}
	if kind == qwenCLIProvider {
		bin = strings.TrimSpace(c.QwenCLIPath)
		if bin == "" {
			bin = "qwen"
		}
		return bin, "npm install -g @qwen-code/qwen-code"
	}
	bin = strings.TrimSpace(c.ClaudeCLIPath)
	if bin == "" {
		bin = "claude"
	}
	return bin, "npm install -g @anthropic-ai/claude-code"
}

// LocalCLIAvailable reports whether the provider's binary exists on this
// machine. The UI calls it to decide whether to offer the provider at all, so
// it must stay cheap and must not start a process.
func (c *Config) LocalCLIAvailable(provider string) bool {
	kind := localCLIKind(provider)
	if kind == "" {
		return false
	}
	bin, _ := c.localCLIBinary(kind)
	_, err := exec.LookPath(bin)
	return err == nil
}

// localCLIModel returns the model configured for a CLI provider. Empty is the
// right default: a subscription already has a model policy of its own, and
// forcing one here is how a user ends up billed for something they did not
// pick.
func (c *Config) localCLIModel(kind string) string {
	if kind == codexCLIProvider {
		return strings.TrimSpace(c.CodexCLIModel)
	}
	if kind == qwenCLIProvider {
		return strings.TrimSpace(c.QwenCLIModel)
	}
	return strings.TrimSpace(c.ClaudeCLIModel)
}

// flattenMessages collapses the package's OpenAI-shaped conversation into the
// one prompt string a CLI accepts. System turns are merged into their own block
// because both CLIs take a system prompt separately; everything else keeps its
// order and is labelled with its role, which is all the model needs to tell an
// earlier answer from a new question.
func flattenMessages(msgs []Message) (system string, prompt string) {
	var systems, turns []string
	for _, m := range msgs {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" {
			systems = append(systems, content)
			continue
		}
		if role == "" {
			role = "user"
		}
		turns = append(turns, role+": "+content)
	}
	return strings.Join(systems, "\n\n"), strings.Join(turns, "\n\n")
}

// localCLIComplete runs the user's own CLI and returns its answer. kind is
// "claude-cli" or "codex-cli".
func (c *Config) localCLIComplete(ctx context.Context, req LLMRequest, kind string) (*LLMResponse, error) {
	// A CLI takes a prompt, not an OpenAI tool schema, so there is no honest way
	// to serve a tool-calling node with one. Dropping the tools silently would
	// leave the Brain waiting for calls that can never arrive, so this fails
	// loudly instead.
	if len(req.Tools) > 0 {
		return nil, &ProviderError{
			Code: "unsupported",
			Message: fmt.Sprintf("%s cannot serve tool-calling nodes: local CLI providers accept a prompt, not a tool schema. "+
				"The Brain node needs an API provider (anthropic, openai or ollama).", kind),
		}
	}

	system, prompt := flattenMessages(req.Messages)
	if prompt == "" && system == "" {
		return nil, fmt.Errorf("no messages to send")
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.localCLIModel(kind)
	}

	switch kind {
	case codexCLIProvider:
		return c.codexCLIComplete(ctx, system, prompt, model)
	case qwenCLIProvider:
		return c.qwenCLIComplete(ctx, system, prompt, model)
	}
	return c.claudeCLIComplete(ctx, system, prompt, model)
}

// ---------- claude (Claude Code) ----------

// claudeCLIEnvelope is the single JSON object `--output-format json` prints
// once the run finishes. Only the fields that decide success and carry the
// answer are decoded; the rest (cost, session id, timings) is not our business.
type claudeCLIEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// resultEnvelope digs the run's verdict out of whatever the CLI printed.
//
// Three shapes, one answer, because two harnesses print the same envelope in
// two wrappings and a second parser would be a second thing to get wrong:
//
//   - one object          `claude -p --output-format json`
//   - an array of events  `qwen -o json`
//   - one event per line  either CLI in stream-json
//
// Only the `result` event decides. The assistant events carry the same text,
// but a run that narrates three turns has three of them, and the last word of
// the run is the one the node asked for.
func resultEnvelope(stdout string) (claudeCLIEnvelope, bool) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return claudeCLIEnvelope{}, false
	}

	// An array of events: the last verdict wins, for the same reason the last
	// agent message does on the codex path — a run can report more than once.
	if strings.HasPrefix(trimmed, "[") {
		var events []claudeCLIEnvelope
		if json.Unmarshal([]byte(trimmed), &events) == nil {
			for i := len(events) - 1; i >= 0; i-- {
				if events[i].Type == "result" {
					return events[i], true
				}
			}
		}
		return claudeCLIEnvelope{}, false
	}

	var env claudeCLIEnvelope
	if json.Unmarshal([]byte(trimmed), &env) == nil {
		return env, true
	}

	// One event per line. Lines that are not JSON are not an error here: the
	// CLIs mix warnings into the stream and always have.
	var last claudeCLIEnvelope
	found := false
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev claudeCLIEnvelope
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != "result" {
			continue
		}
		last, found = ev, true
	}
	return last, found
}

func (c *Config) claudeCLIComplete(ctx context.Context, system, prompt, model string) (*LLMResponse, error) {
	args := []string{"-p", "--output-format", "json"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if system != "" {
		args = append(args, "--append-system-prompt", system)
	}

	stdout, err := c.runLocalCLI(ctx, claudeCLIProvider, args, prompt)
	if err != nil {
		return nil, err
	}

	env, parsed := resultEnvelope(stdout)
	if !parsed {
		// The envelope is not a stable contract. A newer CLI that prints plain
		// text still produced a usable answer, so it is handed back rather than
		// thrown away.
		return &LLMResponse{Content: strings.TrimSpace(stdout)}, nil
	}
	// On failure the CLI reuses the result field for the reason, so it is the
	// error message.
	if env.IsError {
		return nil, &ProviderError{Code: "cli_error", Message: truncate(strings.TrimSpace(env.Result), localCLIStderrCap)}
	}
	if env.Type == "result" && env.Subtype == "success" {
		return &LLMResponse{Content: strings.TrimSpace(env.Result)}, nil
	}
	if s := strings.TrimSpace(env.Result); s != "" {
		return &LLMResponse{Content: s}, nil
	}
	return &LLMResponse{Content: strings.TrimSpace(stdout)}, nil
}

// ---------- codex (Codex CLI) ----------

// codexCLIEvent covers the event shapes seen from `codex exec --json`. The
// format has changed between releases and is not documented as stable, so all
// three known spellings of "the agent said something" are accepted and the rest
// of the stream is ignored.
type codexCLIEvent struct {
	Type string `json:"type"`
	Item *struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Message string `json:"message"`
	} `json:"item"`
	Msg *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"msg"`
	// Codex reports failures on stdout as events, not on stderr, so a run that
	// exits non-zero leaves its only explanation here. Without reading these the
	// user sees "exit status 1" and has nothing to act on.
	Message string `json:"message"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
	LastAgentMessage string `json:"last_agent_message"`
}

// codexCLIFailure digs the human-readable sentence out of a Codex error event.
// The message is often a JSON document from the API quoted inside a string, so
// one level of unwrapping usually turns noise into an actionable line such as
// "please upgrade to the latest CLI".
func codexCLIFailure(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var nested struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(raw), &nested) == nil {
		if nested.Error != nil && strings.TrimSpace(nested.Error.Message) != "" {
			return strings.TrimSpace(nested.Error.Message)
		}
		if strings.TrimSpace(nested.Message) != "" {
			return strings.TrimSpace(nested.Message)
		}
	}
	return raw
}

func (c *Config) codexCLIComplete(ctx context.Context, system, prompt, model string) (*LLMResponse, error) {
	// --skip-git-repo-check is required, not optional: Codex refuses to run in
	// a directory that is not a trusted git repository, and a workflow's working
	// directory is frequently neither. Without it every call in a plain folder
	// fails with "Not inside a trusted directory".
	args := []string{"exec", "--json", "--skip-git-repo-check"}
	if model != "" {
		args = append(args, "--model", model)
	}
	// Codex has no system-prompt flag, so the system block leads the prompt.
	// The trailing "-" tells it to read that prompt from stdin.
	args = append(args, "-")
	stdin := prompt
	if system != "" {
		stdin = strings.TrimSpace(system + "\n\n" + prompt)
	}

	stdout, runErr := c.runLocalCLI(ctx, codexCLIProvider, args, stdin)

	var messages, plain, failures []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev codexCLIEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			plain = append(plain, line)
			continue
		}
		if ev.Type == "item.completed" && ev.Item != nil && ev.Item.Type == "agent_message" {
			if s := strings.TrimSpace(ev.Item.Text); s != "" {
				messages = append(messages, s)
			}
		}
		if ev.Msg != nil && ev.Msg.Type == "agent_message" {
			if s := strings.TrimSpace(ev.Msg.Message); s != "" {
				messages = append(messages, s)
			}
		}
		if s := strings.TrimSpace(ev.LastAgentMessage); s != "" {
			messages = append(messages, s)
		}
		switch {
		case ev.Type == "error":
			failures = append(failures, codexCLIFailure(ev.Message))
		case ev.Type == "turn.failed" && ev.Error != nil:
			failures = append(failures, codexCLIFailure(ev.Error.Message))
		case ev.Type == "item.completed" && ev.Item != nil && ev.Item.Type == "error":
			failures = append(failures, codexCLIFailure(ev.Item.Message))
		}
	}

	// The exit status decides which of the two streams to believe. Codex emits
	// advisory error items on a run that goes on to succeed (a model metadata
	// warning, for instance), so an error event alone must not discard a real
	// answer. When the process actually failed, the error event is the only
	// explanation there is, and it outranks the exit code.
	if runErr != nil {
		for i := len(failures) - 1; i >= 0; i-- {
			if reason := strings.TrimSpace(failures[i]); reason != "" {
				return nil, &ProviderError{
					Code:    "cli_error",
					Message: fmt.Sprintf("%s: %s", codexCLIProvider, truncate(reason, localCLIStderrCap)),
				}
			}
		}
		return nil, runErr
	}

	// The run may narrate several turns; the last one is the answer.
	if n := len(messages); n > 0 {
		return &LLMResponse{Content: messages[n-1]}, nil
	}
	// Nothing recognizable in the stream: whatever the CLI printed in plain text
	// is the best answer available.
	return &LLMResponse{Content: strings.TrimSpace(strings.Join(plain, "\n"))}, nil
}

// ---------- qwen (Qwen Code) ----------

// Qwen Code est le troisième harnais, et il n'est pas un troisième du même
// genre : c'est le seul qu'on peut VISER.
//
// claude et codex parlent au dos de leur propre abonnement et n'en changent
// pas. Qwen Code, lui, prend un `--auth-type` — openai, openai-responses,
// anthropic, gemini, vertex-ai, ou son propre compte — et une adresse. Les
// fournisseurs que ce dépôt connaît déjà (Ollama sur cette machine, LM Studio,
// un point d'accès quelconque) deviennent donc des dos d'agent sans qu'on ait
// à écrire la moindre traduction de protocole. C'est la différence entre
// « trois harnais » et « trois harnais × tous nos fournisseurs ».
//
// Vérifié sur cette machine plutôt que supposé : avec `--auth-type anthropic`
// il poste sur `/v1/messages`, avec `--auth-type openai` sur
// `/v1/chat/completions`. Une sonde a lu les deux.
//
// Et son enveloppe de sortie est celle de Claude Code, au mot près — `{"type":
// "result","subtype":"success","is_error":…,"result":…}`. D'où l'absence d'un
// troisième analyseur ici : `resultEnvelope` sert les deux.
func (c *Config) qwenCLIComplete(ctx context.Context, system, prompt, model string) (*LLMResponse, error) {
	// --bare coupe la découverte automatique au démarrage. Ce n'est pas une
	// optimisation de confort : mesuré sur la même question, 190 s avec et 10 s
	// sans, parce que l'extracteur de mémoire lançait deux requêtes de 26 000
	// jetons avant de répondre quoi que ce soit. Un nœud de workflow ne paie
	// pas ça.
	//
	// --approval-mode plan parce qu'un nœud de texte répond, il ne modifie
	// rien : « analyze only, do not modify files or execute commands ». C'est
	// la posture que `claude -p` a déjà par défaut, dite explicitement ici
	// parce que le défaut de Qwen Code, lui, demanderait une approbation que
	// personne ne peut donner à un processus sans terminal.
	args := []string{"--bare", "--approval-mode", "plan", "-o", "json"}
	if model != "" {
		args = append(args, "-m", model)
	}
	if system != "" {
		args = append(args, "--append-system-prompt", system)
	}
	flags, aimEnv := c.qwenCLIAim()
	args = append(args, flags...)

	stdout, err := c.runLocalCLI(ctx, qwenCLIProvider, args, prompt, aimEnv...)
	if err != nil {
		return nil, err
	}

	env, parsed := resultEnvelope(stdout)
	if !parsed {
		return &LLMResponse{Content: strings.TrimSpace(stdout)}, nil
	}
	if env.IsError {
		return nil, &ProviderError{
			Code:    "cli_error",
			Message: fmt.Sprintf("%s: %s", qwenCLIProvider, truncate(strings.TrimSpace(env.Result), localCLIStderrCap)),
		}
	}
	if s := strings.TrimSpace(env.Result); s != "" {
		return &LLMResponse{Content: s}, nil
	}
	return &LLMResponse{Content: strings.TrimSpace(stdout)}, nil
}

// qwenCLIAim points Qwen Code at one of the providers this project already
// has: des drapeaux d'un côté, un environnement de l'autre.
//
// **La clef ne passe pas par la ligne de commande.** `ps` la montrerait à tout
// ce qui tourne sur la machine, et ce fichier prend déjà soin d'écrire la
// question sur stdin pour cette raison exacte — une clef mérite au moins
// autant. Qwen Code lit `OPENAI_BASE_URL` et `OPENAI_API_KEY` dans son
// environnement : vérifié à la sonde, il poste alors sur
// `/v1/chat/completions` avec `Authorization: Bearer …` sans qu'aucun drapeau
// ne les nomme.
//
// Rien n'est visé par défaut : un environnement vide laisse la CLI sur son
// propre compte, ce qu'attend quelqu'un qui l'a installée et s'y est connecté.
// Viser est un acte de configuration, et il lit la MÊME entrée de point d'accès
// que les nœuds de texte — une deuxième idée de « où est LM Studio » serait
// celle qui a tort le jour où le port change.
//
// Seuls les points d'accès compatibles OpenAI sont servis ici. Anthropic et
// OpenAI se joignent de la même façon, mais par une clef que ce moteur détient
// pour son propre compte, et la confier à un sous-processus qui la dépensera
// sous sa propre politique est une décision qui revient à la personne.
func (c *Config) qwenCLIAim() (flags []string, env []string) {
	target := strings.ToLower(strings.TrimSpace(c.QwenCLIEndpoint))
	if target == "" || !isOpenAICompatible(target) {
		return nil, nil
	}
	e := c.endpointFor(target)
	url := strings.TrimSpace(e.URL)
	if url == "" {
		return nil, nil
	}
	key := strings.TrimSpace(e.Key)
	if key == "" {
		// Un serveur local n'en demande pas, mais le client en exige une :
		// sans valeur, Qwen Code réclame une connexion au lieu d'appeler.
		key = "local"
	}
	return []string{"--auth-type", "openai"}, []string{
		"OPENAI_BASE_URL=" + url,
		"OPENAI_API_KEY=" + key,
	}
}

// ---------- process ----------

// runLocalCLI executes one CLI call and returns its stdout. The prompt is
// written to stdin rather than passed as an argument so it never lands in the
// process table, and it is never echoed into an error or a log: it can hold
// anything the workflow touched.
func (c *Config) runLocalCLI(ctx context.Context, kind string, args []string, stdin string, extraEnv ...string) (string, error) {
	bin, install := c.localCLIBinary(kind)

	timeout := c.LocalCLITimeout
	if timeout <= 0 {
		timeout = defaultLocalCLITimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := strings.TrimSpace(c.LocalCLIWorkdir)
	if dir == "" {
		// An agent CLI reads and writes files relative to where it runs, so it is
		// given a scratch directory instead of whatever the server happened to
		// start in.
		dir = os.TempDir()
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = dir
	// The CLI finds its own session in the user's home directory and needs the
	// inherited PATH to locate its helpers, so the environment is passed through
	// deliberately rather than by default.
	//
	// Et c'est par là que passe ce qui ne doit pas se lire dans `ps` : une clef
	// de point d'accès est ajoutée ici, jamais dans les arguments.
	cmd.Env = append(os.Environ(), extraEnv...)

	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}

	// A bare name that is not on PATH comes back as *exec.Error; a configured
	// absolute path that does not exist fails later, in the fork, as ENOENT.
	// Both mean the same thing to the user.
	var execErr *exec.Error
	if errors.As(err, &execErr) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return "", &ProviderError{
			Code: "not_installed",
			Message: fmt.Sprintf("%s: the %q command was not found on this machine. Install it with: %s",
				kind, bin, install),
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		return "", &ProviderError{
			Code:    "cli_error",
			Message: fmt.Sprintf("%s: %s timed out after %s", kind, bin, timeout),
		}
	}
	detail := truncate(strings.TrimSpace(stderr.String()), localCLIStderrCap)
	if detail == "" {
		detail = err.Error()
	}
	// Stdout is returned alongside the error on purpose: Codex reports why a run
	// failed as an event on stdout, and discarding it here would reduce a usable
	// explanation to an exit code.
	return stdout.String(), &ProviderError{
		Code:    "cli_error",
		Message: fmt.Sprintf("%s: %s failed: %s", kind, bin, detail),
	}
}
