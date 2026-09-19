package localstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ce qui appartient à la personne, pas au projet.
//
// Demandé par Jeremy en une phrase qui dit tout : « à chaque projet je dois
// configurer les providers… sinon c'est l'enfer ». Il avait raison, et le
// classement d'origine était faux. Une clef d'API et l'adresse d'un LM Studio
// ne décrivent pas un projet : elles décrivent la machine devant laquelle on
// est assis. Les ranger dans `.zyvro` obligeait à les retaper à chaque dossier
// ouvert, ce qui pour une clef qu'on ne connaît pas par cœur veut dire aller la
// rechercher dans une console.
//
// Ce qui reste au projet, et pour de bonnes raisons : **l'ordre** des
// fournisseurs par métier. Celui-là décrit bien le projet — celui qui coûte
// cher passe par le fournisseur prudent, celui où l'on bricole par le moins
// cher — et il ne coûte rien à réécrire puisqu'il ne se retient pas.
//
// Où exactement : `os.UserConfigDir()`, c'est-à-dire
// `~/Library/Application Support/Zyvro` sur macOS, `%AppData%\Zyvro` sur
// Windows, `~/.config/Zyvro` ailleurs. Demandé au système plutôt qu'écrit à la
// main : trois chemins écrits ici seraient trois occasions de se tromper d'un
// dossier sur un système qu'on ne teste pas.
//
// La règle de lecture, en une phrase : **la machine gagne, le projet sert de
// repli.** Le repli est ce qui fait que rien ne casse — un projet réglé avant
// aujourd'hui garde ses clefs et continue de tourner — et « la machine gagne »
// est ce qui fait que régler une fois suffit ensuite partout.

// machineFile is the one file. Same name as the project's, because it holds the
// same thing for a wider audience, and a second name would be a second idea.
const machineFile = "providers.json"

// MachineDirEnv lets a test — or someone running two installs side by side —
// say where this file lives. Without it every test in this package would write
// into the real configuration of whoever ran it.
const MachineDirEnv = "ZYVRO_CONFIG_DIR"

// machineFileMode: the same 0600 as the project's secrets, for the same reason.
// This file holds plaintext credentials and sits in a home directory that other
// accounts on a shared machine can traverse.
const machineFileMode os.FileMode = 0o600

// Machine is the per-person store: credentials and server addresses, shared by
// every project opened on this machine.
type Machine struct {
	dir string

	mu sync.RWMutex
}

type machineData struct {
	Version   int                     `json:"version"`
	Secrets   map[string]storedSecret `json:"secrets,omitempty"`
	Endpoints map[string]Endpoint     `json:"endpoints,omitempty"`
}

// MachineDir is where this store keeps its file.
func MachineDir() (string, error) {
	if custom := strings.TrimSpace(os.Getenv(MachineDirEnv)); custom != "" {
		return custom, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Zyvro"), nil
}

// OpenMachine prepares the directory. It is not fatal for it to fail: a project
// still opens without it, with everything a project already had. The daemon
// therefore keeps a nil Machine rather than refusing to start.
func OpenMachine() (*Machine, error) {
	dir, err := MachineDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Machine{dir: dir}, nil
}

// Dir is the folder this store writes in, for a panel that wants to say where.
func (m *Machine) Dir() string {
	if m == nil {
		return ""
	}
	return m.dir
}

func (m *Machine) path() string { return filepath.Join(m.dir, machineFile) }

// SecretValues returns provider -> credential.
func (m *Machine) SecretValues() map[string]string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	data := m.read()
	out := make(map[string]string, len(data.Secrets))
	for provider, entry := range data.Secrets {
		if entry.Value != "" {
			out[provider] = entry.Value
		}
	}
	return out
}

// ListSecrets returns the metadata view, sorted by provider.
func (m *Machine) ListSecrets() []Secret {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	data := m.read()
	out := make([]Secret, 0, len(data.Secrets))
	for provider, entry := range data.Secrets {
		out = append(out, secretView(provider, entry))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// SetSecret stores one credential for every project on this machine.
func (m *Machine) SetSecret(provider, secret string) (*Secret, error) {
	if m == nil {
		return nil, errors.New("no machine configuration directory")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !providerPattern.MatchString(provider) {
		return nil, ErrInvalidProvider
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("secret required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	data := m.read()
	if data.Secrets == nil {
		data.Secrets = map[string]storedSecret{}
	}
	entry := storedSecret{Value: secret, UpdatedAt: time.Now().UTC()}
	data.Secrets[provider] = entry
	if err := m.write(data); err != nil {
		return nil, err
	}
	view := secretView(provider, entry)
	return &view, nil
}

// DeleteSecret removes one. Removing one that was never stored is not an error:
// the caller asked for it to be gone, and it is.
func (m *Machine) DeleteSecret(provider string) error {
	if m == nil {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !providerPattern.MatchString(provider) {
		return ErrInvalidProvider
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data := m.read()
	if _, ok := data.Secrets[provider]; !ok {
		return nil
	}
	delete(data.Secrets, provider)
	return m.write(data)
}

// Endpoints is where this machine's local servers are.
func (m *Machine) Endpoints() map[string]Endpoint {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.read().Endpoints
}

// SetEndpoint records one server's address and default model, or forgets it
// when everything is empty — which is what "turn this provider off" means for a
// provider that has no key to delete.
func (m *Machine) SetEndpoint(provider string, e Endpoint) error {
	if m == nil {
		return errors.New("no machine configuration directory")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data := m.read()
	if data.Endpoints == nil {
		data.Endpoints = map[string]Endpoint{}
	}
	if strings.TrimSpace(e.URL) == "" && strings.TrimSpace(e.Model) == "" && strings.TrimSpace(e.Key) == "" {
		delete(data.Endpoints, provider)
	} else {
		data.Endpoints[provider] = e
	}
	return m.write(data)
}

// read treats absent and unreadable alike: nothing configured. A half-written
// file must not decide where calls go, and the person can always type the key
// again — which is a far better outcome than a daemon that refuses to start.
func (m *Machine) read() machineData {
	data := machineData{Version: ProjectVersion}
	raw, err := os.ReadFile(m.path())
	if err != nil {
		return data
	}
	if json.Unmarshal(raw, &data) != nil {
		return machineData{Version: ProjectVersion}
	}
	return data
}

// write is beside-and-rename, like every other write here: two windows open on
// two projects share this file, and a reader landing mid-write must never see a
// truncated one.
//
// Ce qu'il reste comme course : deux fenêtres qui enregistrent une clef à la
// même seconde: la dernière gagne. Un verrou de fichier entre processus
// coûterait plus cher que ce qu'il éviterait — personne ne règle deux
// fournisseurs dans deux fenêtres au même instant, et le pire des cas est une
// clef à retaper, pas un fichier cassé.
func (m *Machine) write(data machineData) error {
	data.Version = ProjectVersion
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	return writeJSONAtomicMode(m.path(), data, machineFileMode)
}
