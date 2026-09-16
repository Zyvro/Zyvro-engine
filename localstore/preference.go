package localstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

// preferences is the file's shape. A map of job to provider ids, best first.
type preferences struct {
	Order map[string][]string `json:"order"`
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
	raw, err := os.ReadFile(s.preferencePath())
	if err != nil {
		return nil
	}
	var parsed preferences
	if json.Unmarshal(raw, &parsed) != nil {
		return nil
	}
	return parsed.Order
}

// SetProviderOrder records it. Written beside and renamed, so a crash halfway
// through cannot leave a truncated file that reads as "no preference".
func (s *Store) SetProviderOrder(order map[string][]string) error {
	if err := os.MkdirAll(s.ZyvroDir(), 0o755); err != nil {
		return err
	}
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

	body, err := json.MarshalIndent(preferences{Order: clean}, "", "  ")
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
