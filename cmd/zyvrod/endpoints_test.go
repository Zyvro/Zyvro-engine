package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/providers"
)

// The local endpoints are the first providers configured by an address instead
// of a key, and that difference is where they can go wrong: the panel has to
// draw them differently, the file has to keep them alongside the ordering, and
// the engine has to actually receive them. Each of those was a separate place
// something could be saved and never read.

func decodeCatalog(t *testing.T, body []byte) map[string]providerInfo {
	t.Helper()
	var parsed struct {
		Providers []providerInfo `json:"providers"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := map[string]providerInfo{}
	for _, p := range parsed.Providers {
		out[p.ID] = p
	}
	return out
}

func TestTheLocalEndpointsAreOfferedForTextAndVision(t *testing.T) {
	e := newTestEnv(t)
	got := decodeCatalog(t, e.do("GET", "/api/providers", nil).Body.Bytes())
	for _, id := range providers.AddressConfiguredProviders {
		p, ok := got[id]
		if !ok {
			t.Fatalf("%s missing from the catalogue", id)
		}
		if !p.Endpoint {
			t.Errorf("%s is not marked as an address provider, so the panel would draw a key box", id)
		}
		// Les trois serveurs OpenAI-compatibles font aussi la complétion de
		// code : c'est la même route, avec un suffixe.
		want := "text,vision,completion"
		if id == providers.CustomImageProvider {
			// The images half of the same API: a different shape, so a
			// different job, and offering it for text would be the "you are
			// covered" lie the vision split was fixed to stop telling.
			want = "image"
		}
		if strings.Join(p.Roles, ",") != want {
			t.Errorf("%s roles: %v", id, p.Roles)
		}
		if p.HasUserKey {
			t.Errorf("%s reads as configured before anybody enabled it", id)
		}
		if p.DefaultURL != providers.DefaultEndpointURL(id) {
			t.Errorf("%s default address: %q", id, p.DefaultURL)
		}
	}
}

func TestSavingAnEndpointReachesTheEngine(t *testing.T) {
	// The failure this rules out is the one the ordering already had: saved to
	// the file, shown in the panel, and never put into the config a run uses.
	e := newTestEnv(t)
	res := e.do("PUT", "/api/providers/lmstudio/endpoint", map[string]string{
		"url": "http://127.0.0.1:1234/v1", "model": "qwen/qwen3-coder-next",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("save: %d %s", res.Code, res.Body)
	}
	got := decodeCatalog(t, res.Body.Bytes())["lmstudio"]
	if got.EndpointURL != "http://127.0.0.1:1234/v1" || got.Model != "qwen/qwen3-coder-next" {
		t.Errorf("catalogue: %+v", got)
	}
	if !got.HasUserKey {
		t.Error("a configured endpoint still reads as unconfigured")
	}

	cfg := e.daemon.providerConfig()
	if cfg.EndpointFor("lmstudio").Model != "qwen/qwen3-coder-next" {
		t.Errorf("the engine never received it: %+v", cfg.EndpointFor("lmstudio"))
	}
	if cfg.ResolvedVisionProvider("lmstudio") != "lmstudio" {
		t.Error("a vision call naming lmstudio would not land there")
	}
}

func TestTheChosenOrderReachesTheEngine(t *testing.T) {
	// Same hole, found while wiring the endpoints: the panel let somebody put
	// their local server first for vision, saved it, and runs kept going to
	// whichever credential happened to exist.
	e := newTestEnv(t)
	if res := e.do("PUT", "/api/providers/ollama-local/endpoint", map[string]string{
		"url": "http://127.0.0.1:11434/v1", "model": "gemma3:4b",
	}); res.Code != http.StatusOK {
		t.Fatalf("save endpoint: %d %s", res.Code, res.Body)
	}
	if res := e.do("PUT", "/api/providers/order", map[string]any{
		"order": map[string][]string{"vision": {"ollama-local", "google"}},
	}); res.Code != http.StatusOK {
		t.Fatalf("save order: %d %s", res.Code, res.Body)
	}

	cfg := e.daemon.providerConfig()
	cfg.GoogleAPIKey = "AIzaPretend"
	if got := cfg.ResolvedVisionProvider(""); got != "ollama-local" {
		t.Errorf("a vision call with no named provider went to %q despite the order", got)
	}
}

func TestClearingAnEndpointTurnsItOff(t *testing.T) {
	// These have no key to delete, so an empty address is how "forget this one"
	// is spelled.
	e := newTestEnv(t)
	e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": "http://127.0.0.1:9000/v1", "model": "klein-2.0"})
	res := e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": "", "model": ""})
	if res.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", res.Code, res.Body)
	}
	if got := decodeCatalog(t, res.Body.Bytes())["custom"]; got.HasUserKey || got.EndpointURL != "" {
		t.Errorf("still configured: %+v", got)
	}
	if e.daemon.providerConfig().EndpointConfigured("custom") {
		t.Error("the engine still holds the cleared endpoint")
	}
}

func TestAnAddressWithoutASchemeIsRefusedUpFront(t *testing.T) {
	// A bare host:port is the likeliest thing to type, and letting it through
	// produces a transport error about a missing scheme at the first run rather
	// than about the box that needs one.
	e := newTestEnv(t)
	res := e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": "127.0.0.1:9000/v1"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected a refusal, got %d %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "http://") {
		t.Errorf("the message does not say what is missing: %s", res.Body)
	}
}

func TestOnlyAddressProvidersHaveAnEndpointAndAModelList(t *testing.T) {
	e := newTestEnv(t)
	if res := e.do("PUT", "/api/providers/google/endpoint", map[string]string{"url": "http://evil.example/v1"}); res.Code != http.StatusBadRequest {
		t.Errorf("google accepted an address: %d", res.Code)
	}
	if res := e.do("GET", "/api/providers/anthropic/models", nil); res.Code != http.StatusBadRequest {
		t.Errorf("anthropic offered a model list: %d", res.Code)
	}
}

func TestTheModelListIsWhateverTheServerAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gemma3:4b"},{"id":"llama3.1:latest"}]}`))
	}))
	t.Cleanup(srv.Close)

	e := newTestEnv(t)
	e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": srv.URL})
	res := e.do("GET", "/api/providers/custom/models", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("models: %d %s", res.Code, res.Body)
	}
	var parsed struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if strings.Join(parsed.Models, ",") != "gemma3:4b,llama3.1:latest" {
		t.Errorf("models: %v", parsed.Models)
	}
}

func TestAServerThatIsNotRunningSaysSoRatherThanFailingBlankly(t *testing.T) {
	e := newTestEnv(t)
	e.do("PUT", "/api/providers/custom/endpoint", map[string]string{"url": "http://127.0.0.1:9/v1"})
	res := e.do("GET", "/api/providers/custom/models", nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("expected a gateway failure, got %d %s", res.Code, res.Body)
	}
	if res.Body.Len() == 0 {
		t.Error("no explanation at all")
	}
}

// La complétion passe par le démon : c'est lui que l'éditeur interroge, une
// fois par frappe au repos. Ce qui casse ici casse vite et souvent.
func TestTheEditorCanAskForACompletion(t *testing.T) {
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"text":"a + b"}]}`))
	}))
	t.Cleanup(srv.Close)

	e := newTestEnv(t)
	if res := e.do("PUT", "/api/providers/lmstudio/endpoint", map[string]string{
		"url": srv.URL, "model": "qwen2.5-coder",
	}); res.Code != http.StatusOK {
		t.Fatalf("endpoint: %d %s", res.Code, res.Body)
	}

	res := e.do("POST", "/api/completion", map[string]any{
		"prefix": "def add(a, b):\n    return ",
		"suffix": "\n\nprint(add(1, 2))",
	})
	if res.Code != http.StatusOK {
		t.Fatalf("completion: %d %s", res.Code, res.Body)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Text != "a + b" {
		t.Errorf("text: %q", body.Text)
	}
	if seen["suffix"] != "\n\nprint(add(1, 2))" {
		t.Errorf("**le suffixe n'a pas traversé le démon** : %v", seen["suffix"])
	}
}

func TestOnlyWhatSurroundsTheCursorIsSent(t *testing.T) {
	// Un fichier de dix mille lignes envoyé à chaque frappe coûterait le temps
	// qu'il met à voyager. Ce qui compte est ce qui entoure le curseur.
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		_, _ = w.Write([]byte(`{"choices":[{"text":""}]}`))
	}))
	t.Cleanup(srv.Close)

	e := newTestEnv(t)
	e.do("PUT", "/api/providers/lmstudio/endpoint", map[string]string{"url": srv.URL, "model": "coder"})

	long := strings.Repeat("x", 9000)
	if res := e.do("POST", "/api/completion", map[string]any{"prefix": long, "suffix": long}); res.Code != http.StatusOK {
		t.Fatalf("%d %s", res.Code, res.Body)
	}
	// En aval, le préfixe s'appelle `prompt` : c'est le nom de la route de
	// complétion d'OpenAI, et le démon parle sa langue une fois la frontière
	// passée.
	prefix, _ := seen["prompt"].(string)
	suffix, _ := seen["suffix"].(string)
	if len(prefix) != 4000 || len(suffix) != 4000 {
		t.Errorf("window: prefix %d, suffix %d", len(prefix), len(suffix))
	}
	// Le préfixe garde sa fin — ce qui touche le curseur — et le suffixe garde
	// son début. Coupés du mauvais côté, ils décriraient un autre endroit du
	// fichier.
	if !strings.HasSuffix(long, prefix) {
		t.Error("the prefix was cut from the wrong end")
	}
	if !strings.HasPrefix(long, suffix) {
		t.Error("the suffix was cut from the wrong end")
	}
}

func TestACompletionWithNothingConfiguredSaysSo(t *testing.T) {
	e := newTestEnv(t)
	res := e.do("POST", "/api/completion", map[string]any{"prefix": "x = ", "suffix": "\n"})
	if res.Code != http.StatusBadGateway {
		t.Fatalf("expected a gateway failure, got %d %s", res.Code, res.Body)
	}
	if res.Body.Len() == 0 {
		t.Error("no explanation at all")
	}
}

// Réenregistrer une adresse ne doit pas effacer le modèle choisi.
//
// Trouvé en vérifiant la complétion sur l'app lancée : le projet affichait
// « aucun modèle choisi » pour un réglage qu'on venait de voir à l'écran. Le
// panneau enregistre parfois une adresse sans renvoyer le modèle — il ne l'a
// pas encore chargé, ou le catalogue l'omet parce qu'il est vide — et l'entrée
// entière était remplacée. Un champ absent n'est pas un champ vide.
func TestSavingAnAddressDoesNotForgetTheModel(t *testing.T) {
	e := newTestEnv(t)
	if res := e.do("PUT", "/api/providers/lmstudio/endpoint", map[string]string{
		"url": "http://127.0.0.1:1234/v1", "model": "qwen2.5-coder",
	}); res.Code != http.StatusOK {
		t.Fatalf("first save: %d %s", res.Code, res.Body)
	}

	// Le panneau renvoie l'adresse seule.
	if res := e.do("PUT", "/api/providers/lmstudio/endpoint", map[string]any{
		"url": "http://127.0.0.1:1234/v1",
	}); res.Code != http.StatusOK {
		t.Fatalf("second save: %d %s", res.Code, res.Body)
	}
	if got := e.daemon.providerConfig().EndpointFor("lmstudio").Model; got != "qwen2.5-coder" {
		t.Errorf("**le modèle a été effacé** : %q", got)
	}

	// Mais un modèle explicitement vidé est bien vidé : c'est ainsi qu'on en
	// change, et le « Disconnect » du panneau envoie les deux champs vides.
	if res := e.do("PUT", "/api/providers/lmstudio/endpoint", map[string]any{
		"url": "http://127.0.0.1:1234/v1", "model": "",
	}); res.Code != http.StatusOK {
		t.Fatalf("clearing: %d %s", res.Code, res.Body)
	}
	if got := e.daemon.providerConfig().EndpointFor("lmstudio").Model; got != "" {
		t.Errorf("an explicit empty model was ignored: %q", got)
	}

	// Et débrancher débranche toujours.
	if res := e.do("PUT", "/api/providers/lmstudio/endpoint", map[string]any{"url": "", "model": ""}); res.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", res.Code, res.Body)
	}
	if e.daemon.providerConfig().EndpointConfigured("lmstudio") {
		t.Error("disconnect no longer disconnects")
	}
}

// Une clé collée dans le panneau doit arriver jusqu'à l'exécution.
//
// Elle n'y arrivait pas pour Black Forest Labs. La clé se stockait, le
// catalogue répondait « Connected » — il lit le magasin — et l'exécution
// répondait « no Black Forest Labs key is configured », parce que le pliage des
// secrets vers la configuration est un switch qui ne la nommait pas. Deux
// listes de fournisseurs, dont une seule était à jour, et la personne au milieu
// sans aucun moyen de comprendre.
//
// Ce test est l'unique liste : il prend le catalogue, garde ceux qui se
// configurent par une clé — ni adresse, ni binaire installé — en stocke une, et
// vérifie que l'exécution la voit. Le prochain fournisseur oublié échouera ici
// plutôt que chez quelqu'un.
func TestEveryKeyProviderReachesTheConfig(t *testing.T) {
	e := newTestEnv(t)
	catalog := decodeCatalog(t, e.do("GET", "/api/providers", nil).Body.Bytes())

	tried := 0
	for _, entry := range catalog {
		if entry.Endpoint || isCLIProvider(entry.ID) {
			// Une adresse n'est pas une clé, et un CLI installé non plus.
			continue
		}
		res := e.do("PUT", "/api/secrets", map[string]string{
			"provider": entry.ID,
			"secret":   "clef-de-test-" + entry.ID,
		})
		if res.Code != http.StatusOK {
			t.Fatalf("stocker la clé de %s : %d %s", entry.ID, res.Code, res.Body)
		}
		tried++

		cfg := e.daemon.providerConfig()
		if got := effectiveCredential(cfg, entry.ID); got != "clef-de-test-"+entry.ID {
			t.Errorf(
				"la clé de %s est stockée mais n'atteint pas l'exécution (vue : %q).\n"+
					"providerConfig ne la plie pas dans la configuration — le panneau dira « Connected » "+
					"et le run dira qu'il n'y a pas de clé.",
				entry.ID, got,
			)
		}
	}
	if tried < 4 {
		t.Fatalf("le catalogue n'a offert que %d fournisseurs à clé : le test ne prouve plus rien", tried)
	}
}
