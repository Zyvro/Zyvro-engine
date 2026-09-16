package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// Manifest describes one signed build of the engine. It is what the desktop
// downloads first and the only thing it trusts to tell it what the binary on
// the other end of the URL should be.
type Manifest struct {
	Version    string `json:"version"`
	Platform   string `json:"platform"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	ReleasedAt string `json:"released_at"`
	Notes      string `json:"notes,omitempty"`
	Signature  string `json:"signature,omitempty"`
}

// canonicalPayload is the exact byte sequence a signature covers: the manifest
// with the "signature" field removed, object keys sorted, and no whitespace
// anywhere.
//
// It takes the raw manifest bytes rather than a Manifest value, and both sign
// and verify call this one function, because a signer and a verifier that
// disagree about bytes is how signature schemes fail silently: the signature
// keeps verifying against the bytes the verifier happened to reconstruct while
// the field the attacker changed was never covered at all. Going through the
// raw document also means a field this version of the tool does not know about
// still ends up under the signature instead of being dropped on the way in.
func canonicalPayload(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Numbers stay as the digits that were written. Decoding 9741810 into a
	// float64 and re-encoding it yields 9.74181e+06, which is the same number
	// and a different signature.
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("manifest is not a JSON object")
	}
	// The signature cannot cover itself.
	delete(obj, "signature")

	var buf bytes.Buffer
	if err := writeCanonical(&buf, obj); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeCanonical emits one value in the canonical form: objects with their
// keys sorted bytewise, arrays in order, and no insignificant whitespace.
func writeCanonical(buf *bytes.Buffer, v any) error {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case json.Number:
		buf.WriteString(val.String())
		return nil
	case string:
		// Delegated so string escaping is the standard library's answer and
		// not a second opinion of our own.
		enc, err := json.Marshal(val)
		if err != nil {
			return err
		}
		buf.Write(enc)
		return nil
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case nil:
		buf.WriteString("null")
		return nil
	default:
		return fmt.Errorf("manifest holds a value of unexpected type %T", v)
	}
}

// signManifest fills in Signature over the canonical payload of everything
// else in the manifest, and returns the document to write to disk.
func signManifest(m Manifest, key ed25519.PrivateKey) ([]byte, error) {
	m.Signature = ""
	unsigned, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	payload, err := canonicalPayload(unsigned)
	if err != nil {
		return nil, err
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))

	// Written indented because a release manifest is read by people during an
	// incident. The signature covers the canonical form, so how this file is
	// laid out on disk does not matter to verification.
	signed, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(signed, '\n'), nil
}

// verifyManifest checks the signature carried by a raw manifest document
// against pub. It returns the parsed manifest so the caller can go on to check
// the binary against it.
func verifyManifest(raw []byte, pub ed25519.PublicKey) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Signature == "" {
		return Manifest{}, errors.New("manifest carries no signature")
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return Manifest{}, fmt.Errorf("decode signature: %w", err)
	}
	payload, err := canonicalPayload(raw)
	if err != nil {
		return Manifest{}, err
	}
	if !ed25519.Verify(pub, payload, sig) {
		return Manifest{}, errors.New("signature does not match this manifest")
	}
	return m, nil
}

// hashBinary returns the size and hex SHA-256 of a release binary. The file is
// streamed rather than read whole: engine builds are tens of megabytes and
// there is no reason to hold one in memory to hash it.
func hashBinary(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}
