// Command zyvrel builds and signs the release manifests for zyvrod, the local
// engine the desktop app ships and updates.
//
// It never runs on the server. The whole point of signing the artifact is that
// a compromise of the host serving it — or of the CDN in front of it — cannot
// turn the update channel into remote code execution on every user's machine,
// and that only holds while the private key lives somewhere the server is not.
// The server stores manifests this tool produced; it cannot produce one.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usage = `zyvrel — sign Zyvro engine releases

  zyvrel keygen --out <dir>
  zyvrel sign   --key <private.pem> --version <v> --binary <path> --platform <os-arch> --out <manifest.json> [--notes <text>] [--released-at <RFC3339>]
  zyvrel verify --pub <public.pem> --manifest <manifest.json> --binary <path>
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = runKeygen(os.Args[2:])
	case "sign":
		err = runSign(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "zyvrel: %v\n", err)
		os.Exit(1)
	}
}

func runKeygen(args []string) error {
	fs := newFlagSet("keygen")
	out := fs.String("out", "", "directory to write the keypair into (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*out) == "" {
		return missing("keygen", "--out")
	}
	priv, pub, err := generateKeypair(*out)
	if err != nil {
		return err
	}
	fmt.Printf("private key: %s (mode 0600 — keep it off the server)\n", priv)
	fmt.Printf("public key:  %s (ship this one with the desktop app)\n", pub)
	return nil
}

func runSign(args []string) error {
	fs := newFlagSet("sign")
	keyPath := fs.String("key", "", "PEM Ed25519 private key (required)")
	version := fs.String("version", "", "version this build is, e.g. 1.4.0 (required)")
	binary := fs.String("binary", "", "path to the built zyvrod binary (required)")
	platform := fs.String("platform", "", "target as <os>-<arch>, e.g. darwin-arm64 (required)")
	out := fs.String("out", "", "manifest.json to write (required)")
	notes := fs.String("notes", "", "optional one-line summary shown to the user")
	releasedAt := fs.String("released-at", "", "release timestamp, RFC3339 (default: now, UTC)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireAll("sign", required{"--key", keyPath}, required{"--version", version},
		required{"--binary", binary}, required{"--platform", platform}, required{"--out", out}); err != nil {
		return err
	}
	// The platform becomes a directory name on the release host, so it is
	// checked here as well as there. A manifest that cannot be filed under a
	// legal path is a mistake worth catching on the machine holding the key
	// rather than on the one serving the download.
	if !validPathSegment(*platform) {
		return fmt.Errorf("platform %q must match ^[a-z0-9-]{1,32}$", *platform)
	}
	if strings.ContainsAny(*notes, "\r\n") {
		return fmt.Errorf("notes must be a single line")
	}

	stamp := strings.TrimSpace(*releasedAt)
	if stamp == "" {
		stamp = time.Now().UTC().Format(time.RFC3339)
	} else if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		return fmt.Errorf("released-at %q is not RFC3339: %w", stamp, err)
	}

	key, err := loadPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	size, sum, err := hashBinary(*binary)
	if err != nil {
		return fmt.Errorf("hash binary: %w", err)
	}

	doc, err := signManifest(Manifest{
		Version:    *version,
		Platform:   *platform,
		Size:       size,
		SHA256:     sum,
		ReleasedAt: stamp,
		Notes:      *notes,
	}, key)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(*out); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s %s (%d bytes, sha256 %s)\n", *version, *platform, size, sum)
	fmt.Printf("manifest: %s\n", *out)
	return nil
}

func runVerify(args []string) error {
	fs := newFlagSet("verify")
	pubPath := fs.String("pub", "", "PEM Ed25519 public key (required)")
	manifestPath := fs.String("manifest", "", "manifest.json to check (required)")
	binary := fs.String("binary", "", "binary the manifest describes (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireAll("verify", required{"--pub", pubPath}, required{"--manifest", manifestPath},
		required{"--binary", binary}); err != nil {
		return err
	}

	pub, err := loadPublicKey(*pubPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	m, err := verifyManifest(raw, pub)
	if err != nil {
		return err
	}
	// The signature says what the binary must be; this says the binary on disk
	// is that one. Checking only the first would verify a manifest that
	// describes a file nobody has.
	size, sum, err := hashBinary(*binary)
	if err != nil {
		return fmt.Errorf("hash binary: %w", err)
	}
	if size != m.Size {
		return fmt.Errorf("binary is %d bytes, manifest says %d", size, m.Size)
	}
	if sum != m.SHA256 {
		return fmt.Errorf("binary sha256 is %s, manifest says %s", sum, m.SHA256)
	}
	fmt.Printf("ok: %s %s, %d bytes, sha256 %s\n", m.Version, m.Platform, m.Size, m.SHA256)
	return nil
}

// validPathSegment is the same allowlist the release endpoint applies to the
// channel and platform it is asked for. It is repeated here rather than shared
// because the two live on opposite sides of the trust boundary — this one is a
// typo check on a release engineer's machine, the one in the API is the thing
// keeping user input off the filesystem — and neither should be able to loosen
// the other by accident.
func validPathSegment(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func missing(cmd, flagName string) error {
	return fmt.Errorf("%s: %s is required", cmd, flagName)
}

// required pairs a flag name with where it landed, so the checks below report
// the first missing flag in the order the usage line lists them rather than in
// whatever order a map happens to iterate.
type required struct {
	name  string
	value *string
}

func requireAll(cmd string, flags ...required) error {
	for _, f := range flags {
		if strings.TrimSpace(*f.value) == "" {
			return missing(cmd, f.name)
		}
	}
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
