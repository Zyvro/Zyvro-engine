package localstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// projectWithNeighbour builds the situation every refusal below is about: a
// project folder, and next to it something the user would not want a workflow
// to touch.
func projectWithNeighbour(t *testing.T) (*FileAccess, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "private")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "id_rsa"), []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	return store.Files(), root, outside
}

// listing is every file under dir, relative and sorted enough to compare.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameListing(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFileAccessRoundTrip(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)

	written, err := files.Write("out/report.txt", []byte("hello from a workflow"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if written != "out/report.txt" {
		t.Fatalf("written path = %q", written)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "out", "report.txt"))
	if err != nil {
		t.Fatalf("the file is not where the node says it is: %v", err)
	}
	if string(onDisk) != "hello from a workflow" {
		t.Fatalf("on disk = %q", onDisk)
	}

	data, mime, err := files.Read("out/report.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello from a workflow" {
		t.Fatalf("read back %q", data)
	}
	if !strings.HasPrefix(mime, "text/plain") {
		t.Fatalf("mime = %q", mime)
	}

	// A second write replaces the first, and leaves no temp file behind.
	if _, err := files.Write("out/report.txt", []byte("second")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "report.txt" {
		t.Fatalf("the atomic write left something behind: %v", listing(t, filepath.Join(root, "out")))
	}
}

// The media type is what tells the engine whether a file is an image, so both
// ways of working it out are pinned.
func TestFileAccessDetectsMediaType(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 40))

	if err := os.WriteFile(filepath.Join(root, "photo.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blob"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ path, want string }{
		{"photo.png", "image/png"},    // from the extension
		{"blob", "image/png"},         // no extension: sniffed from the bytes
		{"notes.md", "text/markdown"}, // whatever the platform says, still text
	} {
		_, mime, err := files.Read(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if !strings.HasPrefix(mime, strings.Split(tc.want, "/")[0]+"/") {
			t.Fatalf("%s: mime = %q, want something like %q", tc.path, mime, tc.want)
		}
		if tc.path != "notes.md" && mime != tc.want {
			t.Fatalf("%s: mime = %q, want %q", tc.path, mime, tc.want)
		}
	}
}

// The refusals. A graph is a document people send each other, so each of these
// is a path someone could ship inside a workflow that looks harmless on the
// canvas. Every case asserts the same two things: the call fails, and nothing
// outside the project folder was touched.
func TestFileAccessRefusesPathsThatLeaveTheProject(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"climbing out with ..", "../private/id_rsa"},
		{"climbing out further", "../../etc/passwd"},
		{"an absolute path", "/etc/passwd"},
		{"an absolute path inside the project's parent", "/tmp/anything.txt"},
		{"a home-relative path", "~/.ssh/id_rsa"},
		{"the project folder itself", "."},
		{"the parent", ".."},
		{"nothing at all", ""},
		{"a path that only looks relative", "out/../../private/id_rsa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, root, outside := projectWithNeighbour(t)
			before := listing(t, outside)
			rootBefore := listing(t, root)

			if _, _, err := files.Read(tc.path); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("read was not refused as unsafe: %v", err)
			}
			if _, err := files.Write(tc.path, []byte("pwned")); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("write was not refused as unsafe: %v", err)
			}
			if after := listing(t, outside); !sameListing(before, after) {
				t.Fatalf("a refused write still created something outside the project: %v -> %v", before, after)
			}
			if after := listing(t, root); !sameListing(rootBefore, after) {
				t.Fatalf("a refused write still created something inside the project: %v -> %v", rootBefore, after)
			}
		})
	}
}

// A symlink is the case the textual check cannot see: the path stays inside
// the project all the way through filepath.Clean, and still lands in ~/.ssh.
func TestFileAccessRefusesSymlinksOutOfTheProject(t *testing.T) {
	files, root, outside := projectWithNeighbour(t)
	if err := os.Symlink(outside, filepath.Join(root, "shortcut")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	before := listing(t, outside)

	if _, _, err := files.Read("shortcut/id_rsa"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("reading through the link was not refused: %v", err)
	}
	if _, err := files.Write("shortcut/planted.txt", []byte("pwned")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("writing through the link was not refused: %v", err)
	}
	// And the same for a link to a single file rather than a directory.
	if err := os.Symlink(filepath.Join(outside, "id_rsa"), filepath.Join(root, "key.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.Read("key.txt"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("reading a link to an outside file was not refused: %v", err)
	}
	if _, err := files.Write("key.txt", []byte("pwned")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("writing a link to an outside file was not refused: %v", err)
	}

	if after := listing(t, outside); !sameListing(before, after) {
		t.Fatalf("something outside the project changed: %v -> %v", before, after)
	}
	if got, err := os.ReadFile(filepath.Join(outside, "id_rsa")); err != nil || string(got) != "PRIVATE KEY" {
		t.Fatalf("the outside file was overwritten through the link: %q %v", got, err)
	}
}

// .zyvro is the store's own folder: the workflows, the run history and the
// plaintext provider keys. A workflow must not be able to rewrite the thing it
// is stored in, or read the credentials out of it into a prompt.
func TestFileAccessRefusesTheZyvroDirectory(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)
	secrets := filepath.Join(root, ".zyvro", "secrets.json")
	if err := os.WriteFile(secrets, []byte(`{"google":"real-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		".zyvro/secrets.json",
		".zyvro",
		".zyvro/workflows/aaaaaaaaaaaaaaaaaaaaaaaa.json",
		"out/../.zyvro/secrets.json",
	} {
		if _, _, err := files.Read(path); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("read %s was not refused: %v", path, err)
		}
		if _, err := files.Write(path, []byte(`{"google":"stolen"}`)); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("write %s was not refused: %v", path, err)
		}
	}
	if got, err := os.ReadFile(secrets); err != nil || string(got) != `{"google":"real-key"}` {
		t.Fatalf("secrets.json = %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".zyvro", "workflows", "aaaaaaaaaaaaaaaaaaaaaaaa.json")); !os.IsNotExist(err) {
		t.Fatal("a refused write still created a file in .zyvro")
	}

	// A link inside the project pointing at .zyvro must not be a way round it.
	if err := os.Symlink(filepath.Join(root, ".zyvro"), filepath.Join(root, "store")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := files.Read("store/secrets.json"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("reading .zyvro through a link was not refused: %v", err)
	}
	if _, err := files.Write("store/secrets.json", []byte("x")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("writing .zyvro through a link was not refused: %v", err)
	}
}

func TestFileAccessWriteWithoutCreateDirs(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)

	if _, err := files.WriteWithDirs("nowhere/a.txt", []byte("x"), false); err == nil {
		t.Fatal("writing into a directory that does not exist must fail when createDirs is off")
	}
	if _, err := os.Stat(filepath.Join(root, "nowhere")); !os.IsNotExist(err) {
		t.Fatal("the directory was created anyway")
	}
	if err := os.MkdirAll(filepath.Join(root, "nowhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := files.WriteWithDirs("nowhere/a.txt", []byte("x"), false); err != nil {
		t.Fatalf("writing into an existing directory: %v", err)
	}
}

func TestFileAccessReadFailures(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)

	if _, _, err := files.Read("missing.txt"); err == nil {
		t.Fatal("a missing file must be an error")
	}
	if err := os.MkdirAll(filepath.Join(root, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.Read("folder"); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("reading a directory: %v", err)
	}

	// A file too big to travel through a node output is refused before it is
	// read, so the bytes are never allocated at all. The file is sparse: this
	// is a test about the size check, not about writing 30 MB.
	big, err := os.Create(filepath.Join(root, "video.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(30 << 20); err != nil {
		t.Fatal(err)
	}
	big.Close()
	if _, _, err := files.Read("video.mp4"); err == nil || !strings.Contains(err.Error(), "MB") {
		t.Fatalf("an oversized file must be refused with its size: %v", err)
	}
}
