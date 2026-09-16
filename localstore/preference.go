package localstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The order this project wants its providers tried in, per job.
//
// Beside the secrets rather than with them: what is stored is an ordering, and
// an ordering spread across the things it orders is one nobody can read in one
// go — and one that loses its meaning the moment a key is removed.
//
// In .zyvro, so it travels with the project. Two projects on one machine can
// want different answers: the one that costs money goes to the careful backend,
// the one you are experimenting in goes to the cheap one.

const preferenceFile = "providers.json"

// preferences is the file's shape: the ordering, and the addresses of the
// servers this project talks to.
//
// One file rather than two because both answer the same question — which
// backend does this project use, and on what terms — and because the endpoints
// are the reason half the orderings exist. Splitting them would mean reading
// two files to know where one call goes.
type preferences struct {
	Order     map[string][]string `json:"order,omitempty"`
	Endpoints map[string]Endpoint `json:"endpoints,omitempty"`
}

// Endpoint is one OpenAI-compatible server: Ollama on this machine, LM Studio,
// or an address the person typed. No key field is required of them — a server
// on your own machine authenticates nobody — but one is carried for the case
// where it is somebody's box on the LAN behind a token.
//
// This mirrors providers.Endpoint rather than importing it, because localstore
// is the file format and the engine is the runtime: the file must keep reading
// the same way when the engine's struct grows a field.
type Endpoint struct {
	URL   string `json:"url,omitempty"`
	Key   string `json:"key,omitempty"`
	Model string `json:"model,omitempty"`
}

func (s *Store) preferencePath() string {
	return filepath.Join(s.ZyvroDir(), preferenceFile)
}

// ProviderOrder is what this project asked for, or nothing.
//
// A missing or unreadable file means no preference, which is the honest reading
// and the safe one: every account that has one credential for a job never had
// to choose, and a half-written file should not decide where calls go.
func (s *Store) ProviderOrder() map[string][]string {
	return s.readPreferences().Order
}

// ProviderEndpoints is where this project's local servers are, or nothing.
func (s *Store) ProviderEndpoints() map[string]Endpoint {
	return s.readPreferences().Endpoints
}

// SetProviderEndpoint records one server's address and default model, or
// forgets it when both are empty — which is what "turn this provider off"
// means for a provider that has no key to delete.
func (s *Store) SetProviderEndpoint(provider string, e Endpoint) error {
	current := s.readPreferences()
	if current.Endpoints == nil {
		current.Endpoints = map[string]Endpoint{}
	}
	if strings.TrimSpace(e.URL) == "" && strings.TrimSpace(e.Model) == "" && strings.TrimSpace(e.Key) == "" {
		delete(current.Endpoints, provider)
	} else {
		current.Endpoints[provider] = e
	}
	return s.writePreferences(current)
}

func (s *Store) readPreferences() preferences {
	raw, err := os.ReadFile(s.preferencePath())
	if err != nil {
		return preferences{}
	}
	var parsed preferences
	if json.Unmarshal(raw, &parsed) != nil {
		return preferences{}
	}
	return parsed
}

// writePreferences writes beside and renames, so a crash halfway through cannot
// leave a truncated file that reads as "nothing configured".
func (s *Store) writePreferences(p preferences) error {
	if err := os.MkdirAll(s.ZyvroDir(), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := s.preferencePath()
	temp := path + ".partial"
	if err := os.WriteFile(temp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// SetProviderOrder records it. Written beside and renamed, so a crash halfway
// through cannot leave a truncated file that reads as "no preference".
func (s *Store) SetProviderOrder(order map[string][]string) error {
	// Sorted keys so the file does not churn between writes that changed
	// nothing: it lives in .zyvro, which is meant to be committed.
	clean := map[string][]string{}
	roles := make([]string, 0, len(order))
	for role := range order {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if len(order[role]) > 0 {
			clean[role] = order[role]
		}
	}
	// Read-modify-write: the endpoints in this file were not the caller's to
	// discard, and dropping them would silently disconnect every local server
	// the moment somebody reordered a list.
	current := s.readPreferences()
	current.Order = clean
	return s.writePreferences(current)
}
