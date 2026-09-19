package localstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newMachine(t *testing.T) *Machine {
	t.Helper()
	// Un sous-dossier qui n'existe pas encore : c'est OpenMachine qui doit le
	// créer, et c'est son mode à lui qu'on veut mesurer — pas celui que
	// t.TempDir se trouve donner.
	t.Setenv(MachineDirEnv, filepath.Join(t.TempDir(), "Zyvro"))
	m, err := OpenMachine()
	if err != nil {
		t.Fatalf("open machine: %v", err)
	}
	return m
}

func TestMachineSecretsRoundTrip(t *testing.T) {
	m := newMachine(t)
	if got := m.SecretValues(); len(got) != 0 {
		t.Fatalf("une machine neuve a déjà des clefs : %v", got)
	}
	if _, err := m.SetSecret("google", "AIza-1234"); err != nil {
		t.Fatal(err)
	}
	if got := m.SecretValues()["google"]; got != "AIza-1234" {
		t.Errorf("secret = %q", got)
	}
	list := m.ListSecrets()
	if len(list) != 1 || list[0].Provider != "google" || list[0].SecretLast4 != "1234" {
		t.Fatalf("list = %+v", list)
	}
	if err := m.DeleteSecret("google"); err != nil {
		t.Fatal(err)
	}
	if got := m.SecretValues(); len(got) != 0 {
		t.Errorf("la clef survit à sa suppression : %v", got)
	}
	// Supprimer ce qui n'existe pas est le résultat demandé, pas une erreur.
	if err := m.DeleteSecret("google"); err != nil {
		t.Errorf("seconde suppression : %v", err)
	}
}

// Le fichier est en clair par choix assumé — il n'y a pas de deuxième partie à
// qui cacher la clef sur sa propre machine — donc ses permissions sont ce qui
// empêche un autre compte du même ordinateur de la lire.
func TestMachineFileIsPrivate(t *testing.T) {
	m := newMachine(t)
	if _, err := m.SetSecret("openai", "sk-abcd"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(m.Dir(), "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	dir, err := os.Stat(m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	// Le dossier que nous créons est à nous : 700. Un dossier qui existait déjà
	// garde le sien — le changer serait défaire un réglage que quelqu'un a pu
	// vouloir — et c'est le 600 du fichier qui protège le contenu de toute
	// façon.
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode du dossier = %o, want 700", perm)
	}
}

func TestMachineEndpointsRoundTrip(t *testing.T) {
	m := newMachine(t)
	if err := m.SetEndpoint("lmstudio", Endpoint{URL: "http://127.0.0.1:1234/v1", Model: "qwen"}); err != nil {
		t.Fatal(err)
	}
	if e := m.Endpoints()["lmstudio"]; e.URL != "http://127.0.0.1:1234/v1" || e.Model != "qwen" {
		t.Fatalf("endpoint = %+v", e)
	}
	// Tout vider est la façon de dire « éteins-le » : ces fournisseurs n'ont
	// pas de clef à supprimer.
	if err := m.SetEndpoint("lmstudio", Endpoint{}); err != nil {
		t.Fatal(err)
	}
	if _, still := m.Endpoints()["lmstudio"]; still {
		t.Error("le serveur est encore là après avoir été éteint")
	}
}

// Les clefs et les adresses vivent dans le même fichier et ne se marchent pas
// dessus : enregistrer l'une ne doit pas effacer l'autre.
func TestKeysAndAddressesShareTheFileWithoutErasingEachOther(t *testing.T) {
	m := newMachine(t)
	if _, err := m.SetSecret("google", "AIza-1"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEndpoint("ollama-local", Endpoint{URL: "http://127.0.0.1:11434/v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetSecret("openai", "sk-2"); err != nil {
		t.Fatal(err)
	}
	if got := m.SecretValues(); len(got) != 2 {
		t.Errorf("secrets = %v", got)
	}
	if got := m.Endpoints(); len(got) != 1 {
		t.Errorf("endpoints = %v", got)
	}
}

// Un fichier illisible veut dire « rien de configuré », jamais « le démon ne
// démarre pas ». La personne peut retaper une clef ; elle ne peut rien faire
// d'une application qui refuse d'ouvrir.
func TestAnUnreadableMachineFileReadsAsNothing(t *testing.T) {
	m := newMachine(t)
	if err := os.WriteFile(filepath.Join(m.Dir(), "providers.json"), []byte("{ ceci n'est pas"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := m.SecretValues(); len(got) != 0 {
		t.Errorf("secrets = %v", got)
	}
	if got := m.Endpoints(); len(got) != 0 {
		t.Errorf("endpoints = %v", got)
	}
	// Et l'écriture suivante répare le fichier plutôt que de buter dessus.
	if _, err := m.SetSecret("google", "AIza-9"); err != nil {
		t.Fatalf("écriture après un fichier cassé : %v", err)
	}
	if got := m.SecretValues()["google"]; got != "AIza-9" {
		t.Errorf("secret = %q", got)
	}
}

// Un magasin nul — dossier de configuration impossible à préparer — se lit
// comme vide et ne panique pas : c'est ce qui laisse un projet fonctionner avec
// ce qu'un projet avait déjà.
func TestANilMachineIsSimplyEmpty(t *testing.T) {
	var m *Machine
	if got := m.SecretValues(); len(got) != 0 {
		t.Errorf("secrets = %v", got)
	}
	if got := m.ListSecrets(); len(got) != 0 {
		t.Errorf("list = %v", got)
	}
	if got := m.Endpoints(); len(got) != 0 {
		t.Errorf("endpoints = %v", got)
	}
	if m.Dir() != "" {
		t.Errorf("dir = %q", m.Dir())
	}
	if err := m.DeleteSecret("google"); err != nil {
		t.Errorf("delete = %v", err)
	}
	if _, err := m.SetSecret("google", "x"); err == nil {
		t.Error("enregistrer sans dossier de configuration a réussi")
	}
}

// Le chemin vient du système, pas d'une liste écrite à la main : trois chemins
// écrits ici seraient trois occasions de se tromper de dossier sur un système
// qu'on ne teste pas.
func TestTheMachineDirectoryComesFromTheSystem(t *testing.T) {
	t.Setenv(MachineDirEnv, "")
	dir, err := MachineDir()
	if err != nil {
		t.Skipf("pas de dossier de configuration ici : %v", err)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skip(err)
	}
	if want := filepath.Join(base, "Zyvro"); dir != want {
		t.Errorf("MachineDir() = %q, want %q", dir, want)
	}
	if !strings.HasSuffix(dir, "Zyvro") {
		t.Errorf("le dossier ne porte pas le nom du produit : %q", dir)
	}

	// Et la porte de sortie, qui est ce qui garde les tests hors de la vraie
	// configuration de qui les lance.
	t.Setenv(MachineDirEnv, "/tmp/ailleurs")
	if got, _ := MachineDir(); got != "/tmp/ailleurs" {
		t.Errorf("%s ignoré : %q", MachineDirEnv, got)
	}
}
