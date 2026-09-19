package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Zyvro/Zyvro-engine/localstore"
)

// secondProject ouvre un deuxième dossier sur la même machine : le cas de
// Jeremy, et le seul qui prouve quoi que ce soit ici. Le dossier de
// configuration n'est pas remplacé, parce que c'est justement lui qui est censé
// être commun.
func (e *testEnv) secondProject(t *testing.T) *testEnv {
	t.Helper()
	store, err := localstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open second project: %v", err)
	}
	d, err := newDaemon(store)
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	return &testEnv{t: t, daemon: d, handler: d.handler(), store: store}
}

// « À chaque projet je dois configurer les providers… sinon c'est l'enfer. »
// Une clef réglée dans un dossier est là dans le suivant, sans rien retaper.
func TestAKeyConfiguredOnceIsThereInTheNextProject(t *testing.T) {
	first := newTestEnv(t)
	mustStatus(t, first.do(http.MethodPut, "/api/secrets", map[string]any{
		"provider": "google", "secret": "AIza-shared-9999",
	}), http.StatusOK)

	second := first.secondProject(t)
	secrets := decode[[]localstore.Secret](t, mustStatus(t,
		second.do(http.MethodGet, "/api/secrets", nil), http.StatusOK))
	if len(secrets) != 1 || secrets[0].Provider != "google" {
		t.Fatalf("le deuxième projet ne voit pas la clef : %+v", secrets)
	}
	if secrets[0].Scope != "machine" {
		t.Errorf("scope = %q, want machine", secrets[0].Scope)
	}
	// Et pas seulement à l'écran : c'est la configuration d'un run qui compte.
	if key := second.daemon.providerConfig().GoogleAPIKey; key != "AIza-shared-9999" {
		t.Errorf("un run du deuxième projet a la clef %q", key)
	}
	// Le deuxième projet n'a rien écrit chez lui pour autant.
	if _, err := os.Stat(filepath.Join(second.store.ZyvroDir(), "secrets.json")); err == nil {
		t.Error("le deuxième projet a recopié la clef chez lui")
	}
}

// L'adresse d'un serveur local décrit l'ordinateur, pas le dossier : elle suit
// donc la même règle, et c'est celle qui coûtait le plus cher à retaper.
func TestAnEndpointConfiguredOnceIsThereInTheNextProject(t *testing.T) {
	first := newTestEnv(t)
	mustStatus(t, first.do(http.MethodPut, "/api/providers/lmstudio/endpoint", map[string]any{
		"url": "http://127.0.0.1:1234/v1", "model": "qwen3-coder",
	}), http.StatusOK)

	second := first.secondProject(t)
	cfg := second.daemon.providerConfig()
	e := cfg.EndpointFor("lmstudio")
	if e.URL != "http://127.0.0.1:1234/v1" || e.Model != "qwen3-coder" {
		t.Fatalf("le deuxième projet ne voit pas le serveur : %+v", e)
	}
	if !cfg.EndpointConfigured("lmstudio") {
		t.Error("lmstudio n'est pas considéré comme configuré dans le deuxième projet")
	}
}

// Rien ne casse en chemin : une clef écrite dans un projet avant ce changement
// continue de servir. C'est ce repli qui rend la migration inutile — il n'y a
// rien à déplacer, rien à lancer.
func TestAKeyAlreadyInAProjectStillWorks(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.store.SetSecret("openai", "sk-old-project-key"); err != nil {
		t.Fatal(err)
	}
	if key := e.daemon.providerConfig().OpenAIAPIKey; key != "sk-old-project-key" {
		t.Fatalf("la clef du projet ne sert plus : %q", key)
	}
	secrets := decode[[]localstore.Secret](t, mustStatus(t, e.do(http.MethodGet, "/api/secrets", nil), http.StatusOK))
	if len(secrets) != 1 || secrets[0].Scope != "project" {
		t.Fatalf("secrets = %+v", secrets)
	}

	// Et la machine l'emporte dès qu'elle en a une : c'est la moitié qui fait
	// que régler une fois suffit, sinon un vieux projet garderait pour toujours
	// la clef qu'on vient de remplacer.
	mustStatus(t, e.do(http.MethodPut, "/api/secrets", map[string]any{
		"provider": "openai", "secret": "sk-new-machine-key",
	}), http.StatusOK)
	if key := e.daemon.providerConfig().OpenAIAPIKey; key != "sk-new-machine-key" {
		t.Errorf("la nouvelle clef ne gagne pas : %q", key)
	}
	secrets = decode[[]localstore.Secret](t, mustStatus(t, e.do(http.MethodGet, "/api/secrets", nil), http.StatusOK))
	if len(secrets) != 1 || secrets[0].Scope != "machine" {
		t.Fatalf("secrets = %+v", secrets)
	}
}

// « Supprime » veut dire supprimé. N'effacer que celle de la machine ferait
// reparaître celle du projet, et quelqu'un qui vient de supprimer une clef la
// reverrait à l'écran.
func TestDeletingAKeyClearsBothPlaces(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.store.SetSecret("openai", "sk-old-project-key"); err != nil {
		t.Fatal(err)
	}
	mustStatus(t, e.do(http.MethodPut, "/api/secrets", map[string]any{
		"provider": "openai", "secret": "sk-new-machine-key",
	}), http.StatusOK)

	mustStatus(t, e.do(http.MethodDelete, "/api/secrets/openai", nil), http.StatusOK)

	if key := e.daemon.providerConfig().OpenAIAPIKey; key != "" {
		t.Errorf("une clef survit à sa suppression : %q", key)
	}
	secrets := decode[[]localstore.Secret](t, mustStatus(t, e.do(http.MethodGet, "/api/secrets", nil), http.StatusOK))
	if len(secrets) != 0 {
		t.Errorf("secrets = %+v", secrets)
	}
}

// Éteindre un serveur local vaut pour les deux endroits, pour la même raison.
func TestTurningAnEndpointOffClearsBothPlaces(t *testing.T) {
	e := newTestEnv(t)
	if err := e.store.SetProviderEndpoint("lmstudio", localstore.Endpoint{URL: "http://127.0.0.1:9999/v1"}); err != nil {
		t.Fatal(err)
	}
	mustStatus(t, e.do(http.MethodPut, "/api/providers/lmstudio/endpoint", map[string]any{
		"url": "http://127.0.0.1:1234/v1",
	}), http.StatusOK)
	mustStatus(t, e.do(http.MethodPut, "/api/providers/lmstudio/endpoint", map[string]any{
		"url": "", "model": "", "key": "",
	}), http.StatusOK)

	if e.daemon.providerConfig().EndpointConfigured("lmstudio") {
		t.Error("le serveur du projet est revenu après avoir été éteint")
	}
}

// L'ordre, lui, reste au projet : il décrit bien le projet, et il ne coûte rien
// à réécrire puisqu'il ne se retient pas.
func TestTheProviderOrderStaysWithTheProject(t *testing.T) {
	first := newTestEnv(t)
	mustStatus(t, first.do(http.MethodPut, "/api/providers/order", map[string]any{
		"order": map[string][]string{"text": {"ollama"}},
	}), http.StatusOK)

	second := first.secondProject(t)
	if order := second.store.ProviderOrder(); len(order) != 0 {
		t.Errorf("le deuxième projet a hérité de l'ordre du premier : %v", order)
	}
}

// Et le panneau peut dire où tout cela est rangé, plutôt que de laisser croire
// que ça vit dans le projet.
func TestTheCatalogSaysWhereTheMachineConfigurationLives(t *testing.T) {
	e := newTestEnv(t)
	body := decode[struct {
		MachineDir string `json:"machine_dir"`
	}](t, mustStatus(t, e.do(http.MethodGet, "/api/providers", nil), http.StatusOK))
	if body.MachineDir == "" {
		t.Fatal("machine_dir est vide")
	}
	if body.MachineDir != e.daemon.machine.Dir() {
		t.Errorf("machine_dir = %q", body.MachineDir)
	}
}
