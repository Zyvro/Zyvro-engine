package localstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts a file in the project, making its folders.
func write(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func joined(paths []string) string { return strings.Join(paths, ",") }

// The plain case, and the two properties a batch depends on: only files, and
// in a stable order. A batch is 519 runs; the order they happen in is the
// order someone reads their output folder in afterwards.
func TestListReturnsFilesInOrder(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)
	write(t, root, "sprites/53013.png", "a")
	write(t, root, "sprites/10021.png", "b")
	write(t, root, "sprites/notes.txt", "c")
	if err := os.MkdirAll(filepath.Join(root, "sprites", "old"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := files.List("sprites", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := "sprites/10021.png,sprites/53013.png,sprites/notes.txt"
	if joined(got) != want {
		t.Errorf("list = %q, want %q (folders left out, sorted)", joined(got), want)
	}
}

// Not recursive by default: "run this over sprites/" must not walk into a
// backup folder somebody left there.
func TestListRecursesOnlyWhenAsked(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)
	write(t, root, "sprites/a.png", "a")
	write(t, root, "sprites/old/b.png", "b")

	flat, err := files.List("sprites", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if joined(flat) != "sprites/a.png" {
		t.Errorf("flat list = %q, want just the top level", joined(flat))
	}
	deep, err := files.List("sprites", true)
	if err != nil {
		t.Fatalf("list recursive: %v", err)
	}
	if joined(deep) != "sprites/a.png,sprites/old/b.png" {
		t.Errorf("recursive list = %q", joined(deep))
	}
}

// The root itself is a legitimate folder to run a batch over, and it is
// exactly the path resolve() refuses for Read and Write.
func TestListAcceptsTheProjectRoot(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)
	write(t, root, "one.txt", "1")
	write(t, root, "two.txt", "2")

	for _, rel := range []string{"", ".", "./"} {
		got, err := files.List(rel, false)
		if err != nil {
			t.Fatalf("list %q: %v", rel, err)
		}
		if joined(got) != "one.txt,two.txt" {
			t.Errorf("list %q = %q", rel, joined(got))
		}
	}
}

// .zyvro is this store itself: workflows, secrets and every past run. A
// listing is how a shared workflow would find out what is in it.
func TestListNeverShowsTheZyvroDirectory(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)
	write(t, root, "keep.txt", "x")

	got, err := files.List("", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, p := range got {
		if strings.HasPrefix(p, ".zyvro") {
			t.Fatalf("listed %s: the store's own directory is in the results", p)
		}
	}
	if joined(got) != "keep.txt" {
		t.Errorf("list = %q, want only the project's own file", joined(got))
	}
	if _, err := files.List(".zyvro", false); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("listing .zyvro directly = %v, want ErrUnsafePath", err)
	}
}

// The check that matters, and the same one Read and Write make: a symlink
// committed to a repository is part of the project to git and a way out of it
// to the filesystem.
func TestListSkipsLinksOutOfTheProject(t *testing.T) {
	files, root, outside := projectWithNeighbour(t)
	write(t, root, "assets/real.png", "x")
	if err := os.Symlink(filepath.Join(outside, "id_rsa"), filepath.Join(root, "assets", "borrowed.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "assets", "elsewhere")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := files.List("assets", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if joined(got) != "assets/real.png" {
		t.Errorf("list = %q, want only the file that really is in the project", joined(got))
	}
}

// Every refusal Read and Write make about where a path points, a listing makes
// too — otherwise the listing is the way round them.
func TestListRefusesPathsThatLeaveTheProject(t *testing.T) {
	files, _, _ := projectWithNeighbour(t)
	for _, rel := range []string{"..", "../private", "/etc", "~/", "sprites/../.."} {
		if _, err := files.List(rel, false); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("list %q = %v, want ErrUnsafePath", rel, err)
		}
	}
}

func TestListRefusesAFileAndAMissingFolder(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)
	write(t, root, "notes.txt", "x")

	if _, err := files.List("notes.txt", false); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("listing a file = %v, want a refusal saying it is a file", err)
	}
	if _, err := files.List("nowhere", false); err == nil || !strings.Contains(err.Error(), "no such folder") {
		t.Errorf("listing a missing folder = %v", err)
	}
}

// An empty folder is an empty list, not an error and not nil: it is the answer
// to "what is in here", and a batch says "no files to run over" from it.
func TestListOfAnEmptyFolderIsEmpty(t *testing.T) {
	files, root, _ := projectWithNeighbour(t)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := files.List("empty", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("list = %#v, want an empty slice", got)
	}
}
