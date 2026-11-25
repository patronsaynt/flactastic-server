package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRootConfinement(t *testing.T) {
	dir := t.TempDir()
	lib, outside := filepath.Join(dir, "lib"), filepath.Join(dir, "outside")
	os.MkdirAll(filepath.Join(lib, "a"), 0o755)
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret.flac"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(lib, "a", "in.flac"), []byte("y"), 0o644)
	os.Symlink(outside, filepath.Join(lib, "out"))
	os.Symlink(filepath.Join(outside, "secret.flac"), filepath.Join(lib, "a", "link.flac"))
	os.Symlink("in.flac", filepath.Join(lib, "a", "inside.flac"))
	r, err := openRoot(lib)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../outside/secret.flac", "out/secret.flac", "a/link.flac", "/etc/passwd",
		"a/../../outside/secret.flac", "", "a/\x00"} {
		if f, err := r.Open(rel, unix.O_RDONLY, 0); err == nil {
			f.Close()
			t.Errorf("opened %q", rel)
		}
	}
	// Symlinks are refused even inside the library, as the scanner ignores them.
	if f, err := r.Open("a/inside.flac", unix.O_RDONLY, 0); err == nil {
		f.Close()
		t.Error("followed a symlink")
	}
	if _, err := r.Create("out/new.flac"); err == nil {
		t.Error("created through a symlink out of the library")
	}
	if err := r.Rename("a/in.flac", "out/moved.flac", true); err == nil {
		t.Error("renamed out of the library")
	}
	if err := r.Rename("a/in.flac", "b/c/moved.flac", true); err != nil {
		t.Errorf("rename with new parents: %v", err)
	}
	f, _ := r.Create("x.flac")
	f.Close()
	if err := r.Rename("b/c/moved.flac", "x.flac", true); !errors.Is(err, unix.EEXIST) {
		t.Errorf("noReplace overwrote: %v", err)
	}
	if !r.Exists("x.flac") || r.Exists("nope.flac") {
		t.Error("Exists")
	}
	if _, err := os.Stat(filepath.Join(outside, "moved.flac")); err == nil {
		t.Error("file escaped")
	}
}

func TestFitComponents(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "é́"
	}
	got := fitComponents("a/" + long + "/" + long + ".flac")
	for _, c := range strings.Split(got, "/") {
		if len(c) > 255 {
			t.Fatal("component too long")
		}
	}
	if filepath.Ext(got) != ".flac" {
		t.Fatalf("extension lost: %s", got)
	}
}

// Network and FUSE filesystems often reject renameat2's flags.
func TestRenameWithoutRenameat2Flags(t *testing.T) {
	defer func(f func(int, string, int, string, uint) error) { renameat2 = f }(renameat2)
	renameat2 = func(int, string, int, string, uint) error { return unix.EINVAL }
	dir := t.TempDir()
	for _, n := range []string{"a.flac", "b.flac"} {
		os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644)
	}
	r, err := openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Rename("a.flac", "b.flac", true); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("noReplace onto an existing file: %v", err)
	}
	if err := r.Rename("a.flac", "sub/c.flac", true); err != nil {
		t.Fatalf("noReplace onto a free name: %v", err)
	}
	if err := r.Rename("sub/c.flac", "b.flac", false); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b.flac")); string(b) != "a.flac" {
		t.Fatalf("b.flac holds %q", b)
	}
}

func TestProbeFS(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "Artist/Album"), 0o755)
	lines, err := ProbeFS(dir, "Artist/Album")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		t.Log(l)
		if !strings.HasPrefix(l, "ok") && !strings.Contains(l, "xattr") { // tmpfs may lack user xattrs
			t.Errorf("on a local disk: %s", l)
		}
	}
	left, _ := os.ReadDir(filepath.Join(dir, "Artist/Album"))
	incoming, _ := os.ReadDir(filepath.Join(dir, ".flactastic/incoming"))
	if len(left)+len(incoming) != 0 {
		t.Errorf("left behind: %v %v", left, incoming)
	}
}
