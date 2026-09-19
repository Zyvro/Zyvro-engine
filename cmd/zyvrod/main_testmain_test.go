package main

import (
	"os"
	"testing"

	"github.com/Zyvro/Zyvro-engine/localstore"
)

// Aucun test de ce paquet n'écrit dans la vraie configuration de la personne
// qui le lance.
//
// Ce n'est pas une précaution de principe : depuis que les clefs et les
// adresses des serveurs vivent dans le dossier de configuration de la machine,
// un `newTestEnv` sans cette ligne écrirait dans le `~/Library/Application
// Support/Zyvro` de celui qui tape `go test` — et un test qui supprime une clef
// supprimerait la sienne.
//
// Dans un TestMain plutôt que dans chaque test : c'est une propriété du paquet
// entier, et un test ajouté demain l'aurait oubliée.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zyvro-config-test")
	if err != nil {
		panic("cannot make a temporary configuration directory: " + err.Error())
	}
	if err := os.Setenv(localstore.MachineDirEnv, dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
