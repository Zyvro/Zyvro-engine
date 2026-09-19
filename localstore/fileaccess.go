package localstore

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileAccess implements engine.FileAccess against the project folder itself,
// not against .zyvro. The point of the file nodes is the user's own files —
// the images, prompts and data sitting next to the workflow that reads them —
// so the root is the folder they opened and .zyvro is carved out of it rather
// than being the whole world.
//
// Every path handed to these methods comes out of a workflow graph, and a graph
// is a document people send each other. So the path checks here are not
// defensive tidying: they are the only thing between a shared workflow and the
// rest of the machine it is opened on.
type FileAccess struct {
	root string
}

// Files returns the project folder as a file sink for the engine.
func (s *Store) Files() *FileAccess { return &FileAccess{root: s.Root} }

// Root is the folder every path is resolved against.
func (f *FileAccess) Root() string { return f.root }

// ErrUnsafePath is every refusal that has to do with where a path points. One
// sentinel for all of them on purpose: a caller must not be able to tell a path
// that escaped the root from one that hit .zyvro, or the error becomes a probe.
var ErrUnsafePath = errors.New("unsafe path")

// maxReadBytes stops a huge file from being pulled into memory at all. The
// engine caps a file node's input at the same 25 MB, but it can only do so
// after the bytes exist; checking the size here means a 200 MB video is never
// allocated in the first place.
const maxReadBytes = 25 << 20

// Read returns a file's bytes and its media type.
func (f *FileAccess) Read(rel string) ([]byte, string, error) {
	abs, _, err := f.resolve(rel)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("no such file in the project folder")
		}
		return nil, "", err
	}
	if info.IsDir() {
		return nil, "", fmt.Errorf("%s is a directory, not a file", filepath.Base(abs))
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s is not a regular file", filepath.Base(abs))
	}
	if info.Size() > maxReadBytes {
		return nil, "", fmt.Errorf("file is %.1f MB, over the %d MB a workflow can read",
			float64(info.Size())/(1<<20), maxReadBytes>>20)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, "", err
	}
	return data, detectMime(abs, data), nil
}

// Write creates or replaces a file, creating missing parent directories.
func (f *FileAccess) Write(rel string, data []byte) (string, error) {
	return f.WriteWithDirs(rel, data, true)
}

// WriteWithDirs is Write with the fileOutput node's createDirs setting. With it
// off, a path into a directory that does not exist is an error rather than a
// tree of new folders: someone who mistyped "out/" wants to be told, not to
// find their file in a folder they did not mean to create.
//
// The write is atomic — a temp file in the same directory, then a rename — so a
// workflow that fails while writing cannot leave a half-written file where the
// previous good one was.
func (f *FileAccess) WriteWithDirs(rel string, data []byte, createDirs bool) (string, error) {
	abs, clean, err := f.resolve(rel)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
		return "", fmt.Errorf("%s is a directory", clean)
	}
	dir := filepath.Dir(abs)
	if createDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create directory: %w", err)
		}
	} else if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("directory %s does not exist (turn on createDirs to have it made)", filepath.ToSlash(filepath.Dir(clean)))
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(abs)+".*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	// Every exit removes the temp file; after a successful rename the name is
	// gone already and the remove is a no-op.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	// fsync before the rename, for the same reason the workflow store does it:
	// a rename that lands before the bytes do leaves an empty file behind.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return "", err
	}
	return filepath.ToSlash(clean), nil
}

// resolve turns a workflow-supplied relative path into an absolute one, or
// refuses it. It returns the cleaned relative path as well, which is what the
// node reports as the path it wrote.
//
// Three separate checks, because each catches something the others do not:
// the textual one rejects "../.." before it ever touches the disk, the symlink
// one catches a link inside the project that points at ~/.ssh, and the .zyvro
// one keeps a workflow from rewriting the store it is itself stored in.
func (f *FileAccess) resolve(rel string) (abs string, clean string, err error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", "", fmt.Errorf("%w: no path given", ErrUnsafePath)
	}
	if strings.ContainsRune(rel, 0) {
		return "", "", fmt.Errorf("%w: path contains a NUL byte", ErrUnsafePath)
	}
	if strings.HasPrefix(rel, "~") {
		// Never expanded: a workflow asking for ~/.ssh/id_rsa is exactly the
		// case this whole function exists for.
		return "", "", fmt.Errorf("%w: paths are relative to the project folder", ErrUnsafePath)
	}
	native := filepath.FromSlash(rel)
	if filepath.IsAbs(native) || strings.HasPrefix(rel, "/") || filepath.VolumeName(native) != "" {
		return "", "", fmt.Errorf("%w: paths are relative to the project folder", ErrUnsafePath)
	}
	clean = filepath.Clean(native)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("%w: paths are relative to the project folder", ErrUnsafePath)
	}

	// The root is resolved too: on macOS a project under /var lives at
	// /private/var, and comparing an unresolved root against a resolved path
	// would reject every legitimate file there.
	root, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		return "", "", fmt.Errorf("resolve project folder: %w", err)
	}
	abs = filepath.Join(root, clean)
	if !within(root, abs) {
		return "", "", fmt.Errorf("%w: paths are relative to the project folder", ErrUnsafePath)
	}
	if within(filepath.Join(root, ".zyvro"), abs) {
		return "", "", fmt.Errorf("%w: .zyvro holds Zyvro's own workflows and secrets", ErrUnsafePath)
	}

	// And again after following symlinks, which is the check that matters: a
	// link committed to the repository is part of the project to git and a way
	// out of it to the filesystem.
	resolved, err := resolveExisting(abs)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", clean, err)
	}
	if !within(root, resolved) {
		return "", "", fmt.Errorf("%w: it leads outside the project folder", ErrUnsafePath)
	}
	if within(filepath.Join(root, ".zyvro"), resolved) {
		return "", "", fmt.Errorf("%w: .zyvro holds Zyvro's own workflows and secrets", ErrUnsafePath)
	}
	return abs, clean, nil
}

// within reports whether path is dir or sits underneath it.
func within(dir, path string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(os.PathSeparator))
}

// resolveExisting is filepath.EvalSymlinks for a path that may not exist yet,
// which is the normal case for a file about to be written: it resolves the
// deepest ancestor that does exist and re-attaches the rest. The leaf being
// missing is safe to leave unresolved — a rename onto a name replaces it,
// including when that name is a dangling symlink, so nothing is ever written
// through a link we did not check.
func resolveExisting(path string) (string, error) {
	cur, rest := path, ""
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err // walked past the filesystem root
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// detectMime prefers the extension, because that is what the user named the
// file and what every other part of the product keys off, and falls back to
// sniffing the bytes for files that have no useful extension.
func detectMime(path string, data []byte) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return stripParams(t)
	}
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	return stripParams(http.DetectContentType(head))
}

func stripParams(t string) string {
	if i := strings.Index(t, ";"); i >= 0 {
		t = t[:i]
	}
	return strings.ToLower(strings.TrimSpace(t))
}

// maxListEntries caps one listing. A project folder can contain a
// node_modules with a few hundred thousand files, and a listing is the first
// step of a batch: every path returned here is a run someone may be about to
// pay for. Refusing with the count is more useful than a slice nobody meant to
// ask for.
const maxListEntries = 20000

// ErrTooManyFiles is a folder too big to list. Separate from ErrUnsafePath
// because it is not a refusal about *where* the path points: the caller can act
// on it by narrowing the folder, which is advice worth giving.
var ErrTooManyFiles = errors.New("too many files")

// List returns the files under rel, relative to the project root, sorted.
//
// Implements engine.DirLister. Files only: a batch loops over files, and a
// folder in that list would be an item that always fails. Symlinks are
// followed to decide whether to include the target, never to walk into it —
// the check is the same one Read and Write use, so a link out of the project
// is skipped rather than listed, and a link back into it cannot make the walk
// loop.
func (f *FileAccess) List(rel string, recursive bool) ([]string, error) {
	root, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		return nil, fmt.Errorf("resolve project folder: %w", err)
	}
	abs, clean, err := f.resolveDir(rel, root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no such folder in the project folder")
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is a file, not a folder", filepath.Base(abs))
	}

	out := []string{}
	add := func(p string) error {
		if !f.listable(root, p) {
			return nil
		}
		if len(out) >= maxListEntries {
			return fmt.Errorf("%w: more than %d files under %s; point this at a narrower folder",
				ErrTooManyFiles, maxListEntries, displayDir(clean))
		}
		rp, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rp))
		return nil
	}

	if !recursive {
		entries, readErr := os.ReadDir(abs)
		if readErr != nil {
			return nil, readErr
		}
		// os.ReadDir sorts by filename, so the result is already in the order
		// a person reading the folder would expect.
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if err := add(filepath.Join(abs, e.Name())); err != nil {
				return nil, err
			}
		}
		return out, nil
	}

	walkErr := filepath.WalkDir(abs, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read is not a reason to fail the whole
			// listing: the files we can see are still the files to work on.
			if e != nil && e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			// .zyvro holds this store itself. Skipping the directory rather
			// than filtering its files keeps a large run history from being
			// walked at all.
			if within(filepath.Join(root, ".zyvro"), p) {
				return filepath.SkipDir
			}
			return nil
		}
		return add(p)
	})
	if walkErr != nil {
		return nil, walkErr
	}
	// WalkDir visits lexically, so out is already sorted; sorting again is the
	// cheap way to be sure of it after the non-recursive branch ever changes.
	sort.Strings(out)
	return out, nil
}

// listable is the same gate Read and Write apply, asked of a path we found
// rather than of one we were given: inside the project, outside .zyvro, and a
// regular file once symlinks are followed.
func (f *FileAccess) listable(root, abs string) bool {
	if within(filepath.Join(root, ".zyvro"), abs) {
		return false
	}
	resolved, err := resolveExisting(abs)
	if err != nil || !within(root, resolved) {
		return false
	}
	if within(filepath.Join(root, ".zyvro"), resolved) {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.Mode().IsRegular()
}

// resolveDir is resolve for a folder. The root itself is a legitimate thing to
// list and resolve refuses it — "." is exactly the path that must never reach
// Read or Write — so the empty case is handled here instead of loosening the
// gate every other path goes through.
func (f *FileAccess) resolveDir(rel, root string) (abs string, clean string, err error) {
	trimmed := strings.TrimSpace(rel)
	if trimmed == "" || trimmed == "." || trimmed == "./" {
		return root, "", nil
	}
	return f.resolve(trimmed)
}

// displayDir names a folder in an error the way the person who typed it would.
func displayDir(clean string) string {
	if clean == "" {
		return "the project folder"
	}
	return filepath.ToSlash(clean)
}
