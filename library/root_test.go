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
