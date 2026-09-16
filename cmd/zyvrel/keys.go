package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The filenames keygen writes. They are fixed so that "the release key" means
// one file and not whatever the person running the command called it.
const (
	privateKeyFile = "zyvrel_ed25519.pem"
	publicKeyFile  = "zyvrel_ed25519.pub.pem"
)

// generateKeypair writes a fresh Ed25519 keypair into dir and returns the two
// paths it wrote.
//
// The private key is created with O_EXCL: refusing to overwrite is not a
// convenience check that could be raced, it is the file creation itself. A
// second keygen into a directory that already holds a release key would
// otherwise retire every signature ever made with the old one, and nobody runs
// keygen twice on purpose.
func generateKeypair(dir string) (privPath, pubPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	privPath = filepath.Join(dir, privateKeyFile)
	pubPath = filepath.Join(dir, publicKeyFile)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", "", err
	}

	// 0600, and only ever 0600: this file is the entire security of the update
	// channel, and a release key readable by every account on a build machine
	// is not a release key.
	f, err := os.OpenFile(privPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", "", fmt.Errorf("%s already exists; refusing to overwrite a private key", privPath)
		}
		return "", "", err
	}
	writeErr := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		// A half-written key is worse than none: it would fail to load later
		// with an error that reads like corruption rather than a failed keygen.
		_ = os.Remove(privPath)
		return "", "", writeErr
	}

	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	pf, err := os.OpenFile(pubPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", "", fmt.Errorf("%s already exists; refusing to overwrite it next to a freshly written private key", pubPath)
		}
		return "", "", err
	}
	_, writeErr = pf.Write(pubPEM)
	if closeErr := pf.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return "", "", writeErr
	}
	return privPath, pubPath, nil
}

// loadPrivateKey reads a PEM-encoded PKCS#8 Ed25519 private key.
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	der, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse private key %s: %w", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is a %T, not an Ed25519 private key", path, key)
	}
	return priv, nil
}

// loadPublicKey reads a PEM-encoded PKIX Ed25519 public key.
func loadPublicKey(path string) (ed25519.PublicKey, error) {
	der, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse public key %s: %w", path, err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s is a %T, not an Ed25519 public key", path, key)
	}
	return pub, nil
}

func readPEM(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s does not contain a PEM block", path)
	}
	return block.Bytes, nil
}
