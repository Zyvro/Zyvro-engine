package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBinary stands in for a build of zyvrod. Nothing here executes it; the
// tool only ever hashes what it is pointed at.
func fakeBinary(t *testing.T, dir string, content string) string {
	t.Helper()
	path := filepath.Join(dir, "zyvrod")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// release sets up a keypair, a binary and a signed manifest: the state every
// test below starts from.
type release struct {
	dir      string
	priv     string
	pub      string
	binary   string
	manifest string
}

func newRelease(t *testing.T) release {
	t.Helper()
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	priv, pub, err := generateKeypair(keys)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	r := release{
		dir:      dir,
		priv:     priv,
		pub:      pub,
		binary:   fakeBinary(t, dir, "the engine, more or less"),
		manifest: filepath.Join(dir, "manifest.json"),
	}
	if err := runSign([]string{
		"--key", r.priv,
		"--version", "1.4.0",
		"--binary", r.binary,
		"--platform", "darwin-arm64",
		"--out", r.manifest,
		"--notes", "faster cold start",
		"--released-at", "2026-09-16T04:00:00Z",
	}); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return r
}

func (r release) verify(t *testing.T) error {
	t.Helper()
	return runVerify([]string{"--pub", r.pub, "--manifest", r.manifest, "--binary", r.binary})
}

func (r release) read(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(r.manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

func (r release) write(t *testing.T, m map[string]any) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(r.manifest, raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestSignThenVerifyRoundTrips(t *testing.T) {
	r := newRelease(t)
	if err := r.verify(t); err != nil {
		t.Fatalf("a freshly signed release failed to verify: %v", err)
	}

	m := r.read(t)
	for field, want := range map[string]string{
		"version":     "1.4.0",
		"platform":    "darwin-arm64",
		"released_at": "2026-09-16T04:00:00Z",
		"notes":       "faster cold start",
	} {
		if got, _ := m[field].(string); got != want {
			t.Errorf("manifest %s = %q, want %q", field, got, want)
		}
	}
	if got := m["size"].(json.Number).String(); got != "24" {
		t.Errorf("manifest size = %s, want 24", got)
	}
	if m["signature"] == "" || m["signature"] == nil {
		t.Error("manifest carries no signature")
	}
	// The size has to survive a round trip as an integer: a manifest that says
	// 9.74181e+06 is a manifest the desktop cannot compare against a file.
	if bytes.Contains(mustRead(t, r.manifest), []byte("e+")) {
		t.Error("manifest wrote a number in exponent form")
	}
}

func TestTamperedBinaryFailsVerification(t *testing.T) {
	// A same-length edit, which is the interesting case: the size in the
	// manifest still matches, so only the hash can catch it.
	r := newRelease(t)
	if err := os.WriteFile(r.binary, []byte("the engine, more or LESS"), 0o755); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := r.verify(t)
	if err == nil {
		t.Fatal("verification accepted a binary that does not match its manifest")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error should name the hash mismatch, got: %v", err)
	}
}

func TestTamperedManifestFieldFails(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(m map[string]any)
	}{
		{"version", func(m map[string]any) { m["version"] = "9.9.9" }},
		{"sha256", func(m map[string]any) { m["sha256"] = "00" }},
		{"size", func(m map[string]any) { m["size"] = json.Number("1") }},
		{"platform", func(m map[string]any) { m["platform"] = "windows-amd64" }},
		{"released_at", func(m map[string]any) { m["released_at"] = "2030-01-01T00:00:00Z" }},
		{"notes", func(m map[string]any) { m["notes"] = "now with a backdoor" }},
		{"added field", func(m map[string]any) { m["url"] = "http://evil.example/zyvrod" }},
		{"dropped field", func(m map[string]any) { delete(m, "notes") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t)
			m := r.read(t)
			tc.tamper(m)
			r.write(t, m)
			if err := r.verify(t); err == nil {
				t.Fatalf("verification accepted a manifest with %s changed after signing", tc.name)
			}
		})
	}
}

func TestSignatureFromADifferentKeyFails(t *testing.T) {
	r := newRelease(t)
	otherPriv, otherPub, err := generateKeypair(filepath.Join(r.dir, "other-keys"))
	if err != nil {
		t.Fatalf("second keygen: %v", err)
	}

	// The manifest the real key signed, checked against the wrong public key.
	if err := runVerify([]string{"--pub", otherPub, "--manifest", r.manifest, "--binary", r.binary}); err == nil {
		t.Fatal("verification accepted a manifest against a public key that did not sign it")
	}

	// And the reverse: a manifest signed by a key nobody ships, checked
	// against the key the desktop app actually carries. This is the attack the
	// whole scheme exists to stop.
	forged := filepath.Join(r.dir, "forged.json")
	if err := runSign([]string{
		"--key", otherPriv, "--version", "1.4.0", "--binary", r.binary,
		"--platform", "darwin-arm64", "--out", forged,
	}); err != nil {
		t.Fatalf("sign with the other key: %v", err)
	}
	if err := runVerify([]string{"--pub", r.pub, "--manifest", forged, "--binary", r.binary}); err == nil {
		t.Fatal("verification accepted a manifest signed by an unknown key")
	}
}

func TestCanonicalPayloadIsStableAcrossKeyOrder(t *testing.T) {
	a := []byte(`{"version":"1.4.0","platform":"darwin-arm64","size":9741810,"sha256":"abc","released_at":"2026-09-16T04:00:00Z"}`)
	b := []byte("{\n  \"sha256\": \"abc\",\n  \"released_at\": \"2026-09-16T04:00:00Z\",\n  \"platform\": \"darwin-arm64\",\n  \"size\": 9741810,\n  \"version\": \"1.4.0\"\n}\n")

	ca, err := canonicalPayload(a)
	if err != nil {
		t.Fatalf("canonicalize a: %v", err)
	}
	cb, err := canonicalPayload(b)
	if err != nil {
		t.Fatalf("canonicalize b: %v", err)
	}
	if !bytes.Equal(ca, cb) {
		t.Fatalf("key order changed the signed bytes:\n a: %s\n b: %s", ca, cb)
	}

	want := `{"platform":"darwin-arm64","released_at":"2026-09-16T04:00:00Z","sha256":"abc","size":9741810,"version":"1.4.0"}`
	if string(ca) != want {
		t.Errorf("canonical form is\n  %s\nwant\n  %s", ca, want)
	}

	// The signature cannot cover itself, and adding one must not change the
	// bytes a verifier reconstructs.
	signed := []byte(`{"version":"1.4.0","signature":"AAAA","platform":"darwin-arm64","size":9741810,"sha256":"abc","released_at":"2026-09-16T04:00:00Z"}`)
	cs, err := canonicalPayload(signed)
	if err != nil {
		t.Fatalf("canonicalize signed: %v", err)
	}
	if !bytes.Equal(ca, cs) {
		t.Errorf("the signature field leaked into the payload it signs:\n %s", cs)
	}
}

// A field this tool does not know about still has to end up under the
// signature, or a future manifest gains a field an attacker can edit freely.
func TestUnknownFieldsAreCovered(t *testing.T) {
	r := newRelease(t)
	pub, err := loadPublicKey(r.pub)
	if err != nil {
		t.Fatalf("load pub: %v", err)
	}
	priv, err := loadPrivateKey(r.priv)
	if err != nil {
		t.Fatalf("load priv: %v", err)
	}

	doc := map[string]any{
		"version": "1.4.0", "platform": "darwin-arm64", "size": json.Number("24"),
		"sha256": "abc", "released_at": "2026-09-16T04:00:00Z", "min_desktop": "2.0.0",
	}
	raw, _ := json.Marshal(doc)
	payload, err := canonicalPayload(raw)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	doc["signature"] = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))

	signed, _ := json.Marshal(doc)
	if _, err := verifyManifest(signed, pub); err != nil {
		t.Fatalf("a manifest with an extra field failed to verify: %v", err)
	}

	doc["min_desktop"] = "0.0.1"
	tampered, _ := json.Marshal(doc)
	if _, err := verifyManifest(tampered, pub); err == nil {
		t.Fatal("an extra field was editable without breaking the signature")
	}
}

func TestKeygenRefusesToOverwriteAPrivateKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	priv, _, err := generateKeypair(dir)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	before := mustRead(t, priv)

	if _, _, err := generateKeypair(dir); err == nil {
		t.Fatal("keygen overwrote an existing private key")
	}
	if !bytes.Equal(before, mustRead(t, priv)) {
		t.Fatal("the existing private key was modified by the refused keygen")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(priv)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("private key mode is %o, want 600", mode)
		}
	}
}

func TestSignRejectsAPlatformThatIsNotAPathSegment(t *testing.T) {
	r := newRelease(t)
	for _, platform := range []string{"../../etc", "/darwin-arm64", "Darwin-ARM64", "", "darwin_arm64"} {
		err := runSign([]string{
			"--key", r.priv, "--version", "1.4.0", "--binary", r.binary,
			"--platform", platform, "--out", filepath.Join(r.dir, "bad.json"),
		})
		if err == nil {
			t.Errorf("sign accepted platform %q", platform)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
