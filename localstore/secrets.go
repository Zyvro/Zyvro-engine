package localstore

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Provider secrets live in .zyvro/secrets.json in plaintext. That is a
// deliberate choice for a single-user local tool: there is no second party to
// hide the key from, and the alternatives (an OS keychain prompt on every run,
// or a passphrase the user has to re-enter) buy nothing against an attacker who
// already has the user's filesystem. What the plaintext does require is the two
// mitigations below, and neither is optional:
//
//   - the file is written 0600, so other accounts on a shared machine cannot
//     read it;
//   - .zyvro/.gitignore excludes it, because .zyvro is meant to be committed
//     and an API key pushed to a public repository is the failure this whole
//     feature would otherwise cause.

// secretsFileMode keeps the key readable only by its owner.
const secretsFileMode os.FileMode = 0o600

// providerPattern bounds what can become a key in the secrets file: provider
// ids are short lowercase identifiers, never anything user-authored.
var providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ErrInvalidProvider rejects a provider id that is not a plain identifier.
var ErrInvalidProvider = errors.New("invalid provider")

// Secret is the metadata view of a stored credential: what the frontend's
// ProviderSecret type expects. The secret itself is never in it, so a listing
// can be handed to the client as-is.
type Secret struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	SecretLast4 string    `json:"secret_last4"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Scope says where this credential is kept: "machine" for the one shared by
	// every project, "project" for one that only this folder has. Shown rather
	// than inferred, because the only thing worse than configuring a key twice
	// is not knowing which of the two is the one being used.
	Scope string `json:"scope,omitempty"`
}

// storedSecret is the on-disk entry, value included.
type storedSecret struct {
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

type secretsFile struct {
	Version int                     `json:"version"`
	Secrets map[string]storedSecret `json:"secrets"`
}

func (s *Store) secretsPath() string { return filepath.Join(s.ZyvroDir(), "secrets.json") }

// ListSecrets returns the stored credentials as metadata, sorted by provider so
// the settings panel does not reshuffle between loads.
func (s *Store) ListSecrets() ([]Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	f, err := s.readSecrets()
	if err != nil {
		return nil, err
	}
	out := make([]Secret, 0, len(f.Secrets))
	for provider, entry := range f.Secrets {
		out = append(out, secretView(provider, entry))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out, nil
}

// SecretValues returns provider -> credential for building a run's provider
// config. It is the only method that hands the plaintext back out.
func (s *Store) SecretValues() (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	f, err := s.readSecrets()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(f.Secrets))
	for provider, entry := range f.Secrets {
		if entry.Value != "" {
			out[provider] = entry.Value
		}
	}
	return out, nil
}

// SetSecret stores or replaces one provider credential.
func (s *Store) SetSecret(provider, secret string) (*Secret, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !providerPattern.MatchString(provider) {
		return nil, ErrInvalidProvider
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("secret required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.readSecrets()
	if err != nil {
		return nil, err
	}
	entry := storedSecret{Value: secret, UpdatedAt: time.Now().UTC()}
	f.Secrets[provider] = entry
	if err := s.writeSecrets(f); err != nil {
		return nil, err
	}
	view := secretView(provider, entry)
	return &view, nil
}

// DeleteSecret removes one provider credential. Deleting one that was never
// stored is not an error: the caller asked for it to be gone, and it is.
func (s *Store) DeleteSecret(provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !providerPattern.MatchString(provider) {
		return ErrInvalidProvider
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.readSecrets()
	if err != nil {
		return err
	}
	if _, ok := f.Secrets[provider]; !ok {
		return nil
	}
	delete(f.Secrets, provider)
	return s.writeSecrets(f)
}

// readSecrets loads the file, treating "absent" as "empty": a project that has
// never had a key stored is the normal case, not an error.
func (s *Store) readSecrets() (*secretsFile, error) {
	f := &secretsFile{Version: ProjectVersion, Secrets: map[string]storedSecret{}}
	err := readJSONFile(s.secretsPath(), f)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	if f.Secrets == nil {
		f.Secrets = map[string]storedSecret{}
	}
	return f, nil
}

func (s *Store) writeSecrets(f *secretsFile) error {
	f.Version = ProjectVersion
	return writeJSONAtomicMode(s.secretsPath(), f, secretsFileMode)
}

// secretView is how much of a credential is safe to show: enough to recognize
// which key is configured, not enough to be worth leaking.
func secretView(provider string, entry storedSecret) Secret {
	last4 := ""
	if len(entry.Value) >= 4 {
		last4 = entry.Value[len(entry.Value)-4:]
	}
	return Secret{
		// The frontend keys its rows by id; the provider is the identity here,
		// so it doubles as the id rather than inventing one that means nothing.
		ID:          provider,
		Provider:    provider,
		SecretLast4: last4,
		UpdatedAt:   entry.UpdatedAt,
	}
}
