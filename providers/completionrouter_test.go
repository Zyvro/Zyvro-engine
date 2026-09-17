package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Compléter du code n'est pas discuter avec un modèle, et c'est tout l'objet de
// ce routeur. Ce qui casse ici casse en silence : une suggestion qui arrive en
// texte fantôme a toujours l'air d'une suggestion, même quand c'est une phrase,
// même quand c'est le mauvais modèle, même quand le curseur n'a rien autour.

type seenCompletion struct {
	path string
	body map[string]any
	auth string
}

func completionServer(t *testing.T, answer string) (*httptest.Server, *seenCompletion) {
	t.Helper()
	seen := &seenCompletion{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path, seen.auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&seen.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"text":` + strconv(answer) + `,"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func strconv(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestACompletionAsksToFillTheMiddle(t *testing.T) {
	srv, seen := completionServer(t, "a + b")
	c := &Config{Endpoints: map[string]Endpoint{
		LMStudioProvider: {URL: srv.URL, Model: "qwen2.5-coder"},
	}}

	got, err := c.CodeCompletion(context.Background(), CompletionRequest{
		Provider: LMStudioProvider,
		Prefix:   "def add(a, b):\n    return ",
		Suffix:   "\n\nprint(add(1, 2))",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a + b" {
		t.Errorf("completion: %q", got)
	}
	// La route de complétion, pas celle du chat : c'est la seule qui prend un
	// `suffix`, et une demande partie sur /chat/completions reviendrait en
	// phrase polie.
	if seen.path != "/completions" {
		t.Errorf("asked %q", seen.path)
	}
	if seen.body["prompt"] != "def add(a, b):\n    return " {
		t.Errorf("prompt: %v", seen.body["prompt"])
	}
	if seen.body["suffix"] != "\n\nprint(add(1, 2))" {
		t.Errorf("**le suffixe n'est pas parti** : %v", seen.body["suffix"])
	}
	if seen.body["messages"] != nil {
		t.Error("une complétion est partie comme une conversation")
	}
	// Zéro : une suggestion qui change toute seule d'une frappe à l'autre est
	// une suggestion qu'on n'ose plus accepter.
	if seen.body["temperature"] != float64(0) {
		t.Errorf("temperature: %v", seen.body["temperature"])
	}
	if seen.body["max_tokens"] != float64(DefaultCompletionTokens) {
		t.Errorf("max_tokens: %v", seen.body["max_tokens"])
	}
}

func TestAModelThatCannotInsertSaysWhatToDo(t *testing.T) {
	// Le cas qui arrivera à tout le monde une fois : presque tous les modèles
	// installés sont des modèles de chat. Vérifié contre l'Ollama de cette
	// machine, qui répond exactement cette phrase.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"registry.ollama.ai/library/llama3.1:latest does not support insert","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)

	c := &Config{Endpoints: map[string]Endpoint{OllamaLocalProvider: {URL: srv.URL, Model: "llama3.1:latest"}}}
	_, err := c.CodeCompletion(context.Background(), CompletionRequest{Prefix: "x = ", Suffix: "\n"})
	if err == nil {
		t.Fatal("a chat model was accepted for completion")
	}
	if !strings.Contains(err.Error(), "code model") || !strings.Contains(err.Error(), "llama3.1") {
		t.Errorf("the message does not say what to change: %v", err)
	}
}

func TestNoModelMeansNoRequest(t *testing.T) {
	srv, seen := completionServer(t, "nope")
	c := &Config{Endpoints: map[string]Endpoint{CustomProvider: {URL: srv.URL}}}
	_, err := c.CodeCompletion(context.Background(), CompletionRequest{Prefix: "x", Suffix: "y"})
	if err == nil || !strings.Contains(err.Error(), "no completion model chosen") {
		t.Fatalf("expected a refusal naming the setting, got %v", err)
	}
	if seen.path != "" {
		t.Error("a request went out with no model")
	}
}

func TestNothingAroundTheCursorAsksNothing(t *testing.T) {
	// Un fichier vide n'a pas de milieu à remplir. Demander quand même ferait
	// écrire un fichier entier au modèle, à chaque frappe, pour rien.
	srv, seen := completionServer(t, "une page entière")
	c := &Config{Endpoints: map[string]Endpoint{CustomProvider: {URL: srv.URL, Model: "coder"}}}
	got, err := c.CodeCompletion(context.Background(), CompletionRequest{Prefix: "   ", Suffix: "\n"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" || seen.path != "" {
		t.Errorf("asked anyway: %q via %q", got, seen.path)
	}
}

func TestACompletionIsTrimmedButKeepsItsIndentation(t *testing.T) {
	// L'indentation fait partie de la suggestion ; les espaces de fin traînent
	// dans le fichier, et les retours de tête décalent tout d'une ligne.
	srv, _ := completionServer(t, "\n    return a + b   \n")
	c := &Config{Endpoints: map[string]Endpoint{CustomProvider: {URL: srv.URL, Model: "coder"}}}
	got, err := c.CodeCompletion(context.Background(), CompletionRequest{Prefix: "def f():", Suffix: ""})
	if err != nil {
		t.Fatal(err)
	}
	if got != "    return a + b" {
		t.Errorf("trimmed to %q", got)
	}
}

func TestCompletionPrefersWhatIsOnThisMachine(t *testing.T) {
	// Une complétion doit revenir en quelques dizaines de millisecondes pour
	// être utile. Le dos hébergé reste possible, il n'est simplement pas le
	// premier choix.
	local := &Config{
		OllamaURL: DefaultOllamaURL,
		Endpoints: map[string]Endpoint{LMStudioProvider: {Model: "qwen2.5-coder"}},
	}
	if got := local.ResolvedCompletionProvider(""); got != LMStudioProvider {
		t.Errorf("completion went to %q", got)
	}

	// Le choix de la personne passe devant.
	ordered := &Config{
		OllamaAPIKey: "k",
		Endpoints:    map[string]Endpoint{LMStudioProvider: {Model: "qwen2.5-coder"}},
		Preference:   Preference{"completion": {"ollama"}},
	}
	if got := ordered.ResolvedCompletionProvider(""); got != "ollama" {
		t.Errorf("the account's order was ignored: %q", got)
	}

	// Et un nœud qui nomme un dos l'obtient.
	if got := local.ResolvedCompletionProvider("ollama"); got != "ollama" {
		t.Errorf("a named provider resolved to %q", got)
	}
}

func TestCompletionIsAJobOfItsOwn(t *testing.T) {
	// Le rôle vit dans la liste unique, sinon le catalogue du démon et le
	// service hébergé le découvriraient chacun de leur côté.
	if len(ProvidersFor("completion")) == 0 {
		t.Fatal("completion is not a role the engine knows")
	}
	for _, p := range OpenAICompatibleProviders {
		if !contains(ProvidersFor("completion"), p) {
			t.Errorf("%s cannot complete code", p)
		}
		if contains(HostedCompletionProviders(), p) {
			t.Errorf("%s is offered for hosted completion, and a server cannot reach a laptop", p)
		}
	}
	// OpenAI n'est pas dans la liste : ses modèles actuels ne servent plus
	// cette route, et l'y mettre promettrait une complétion qui échoue.
	if contains(ProvidersFor("completion"), "openai") {
		t.Error("openai is offered for a route it no longer serves")
	}
}

func TestACompletionNeverInheritsTheChatModel(t *testing.T) {
	// OllamaModel est le nom d'un modèle de chat. Retombé dessus, la
	// complétion reviendrait en phrase : grammaticalement correcte, et qui ne
	// compile pas.
	srv, seen := completionServer(t, "x")
	c := &Config{
		OllamaModel: "llama3.1:latest",
		Endpoints:   map[string]Endpoint{CustomProvider: {URL: srv.URL}},
	}
	_, err := c.CodeCompletion(context.Background(), CompletionRequest{Prefix: "a", Suffix: "b"})
	if err == nil {
		t.Fatal("the chat model was used for a completion")
	}
	if seen.body["model"] != nil {
		t.Errorf("sent %v", seen.body["model"])
	}
}
