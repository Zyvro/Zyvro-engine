package localstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MediaStore implements engine.MediaStore against .zyvro/media. The hosted
// store nests files under <exec>/<node>/, but the daemon serves a single flat
// directory: one local user produces few enough files that a flat directory
// stays fast, and a flat directory has exactly one path to validate when
// something is served back out over /content.
type MediaStore struct {
	dir string
}

// Media returns the store's media sink, which is what a Runtime is handed.
func (s *Store) Media() *MediaStore {
	return &MediaStore{dir: s.MediaDir()}
}

// Dir is the directory the /content route serves from.
func (m *MediaStore) Dir() string { return m.dir }

// SaveMedia writes one file and returns the URL the frontend should load it
// from. The URL is host-relative so it resolves against whatever loopback port
// the daemon picked this run, exactly like the hosted API's /content links
// resolve against the API origin.
func (m *MediaStore) SaveMedia(execID, nodeID, filename string, data []byte, mime string) (string, error) {
	_ = mime // the extension carries the type, same as the hosted store
	if len(data) == 0 {
		return "", errors.New("no media bytes to save")
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return "", fmt.Errorf("create media directory: %w", err)
	}
	name := mediaName(execID, nodeID, filename)
	if err := os.WriteFile(filepath.Join(m.dir, name), data, 0o644); err != nil {
		return "", fmt.Errorf("write media: %w", err)
	}
	return "/content/" + name, nil
}

// mediaName flattens the hosted store's <exec>/<node>/<file> into one filename.
// The execution and node ids keep names from colliding between runs, and every
// segment is reduced to a safe character set because all three arrive from
// data we did not write.
func mediaName(execID, nodeID, filename string) string {
	base := filepath.Base(filepath.Clean(filename))
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return sanitizeSeg(execID) + "-" + sanitizeSeg(nodeID) + "-" + sanitizeSeg(stem) + sanitizeExt(ext)
}

// sanitizeSeg maps anything outside [A-Za-z0-9_-] to an underscore, so a
// segment can never introduce a path separator, a "..", or a leading dot.
func sanitizeSeg(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
	if s == "" {
		return "_"
	}
	return s
}

func sanitizeExt(ext string) string {
	if ext == "" {
		return ".bin"
	}
	return "." + sanitizeSeg(strings.TrimPrefix(ext, "."))
}
