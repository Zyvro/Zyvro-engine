package localstore

import (
	"os"
	"testing"
)

// La même garde que dans cmd/zyvrod, et pour la même raison : un test qui
// ouvrirait la configuration de la machine écrirait dans celle de la personne
// qui lance `go test`.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zyvro-config-test")
	if err != nil {
		panic("cannot make a temporary configuration directory: " + err.Error())
	}
	if err := os.Setenv(MachineDirEnv, dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
