package library

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
	"flactastic/server/store"
	"flactastic/server/taglib"
)

// testLibrary is a scanned library over copies of one test file, with no
// scan loop running: tests decide when the index catches up with the disk.
func testLibrary(t *testing.T, names ...string) (*Library, *Root) {
	t.Helper()
	src, err := os.ReadFile("../testdata/audio/cur-single.flac")
	if err != nil {
		t.Fatal(err)
	}
	root, data := t.TempDir(), t.TempDir()
	old := time.Now().Add(-time.Hour)
	for _, n := range names {
		p := filepath.Join(root, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, src, 0o644)
		os.Chtimes(p, old, old)
	}
	id, _ := EnsureIdentity(root)
	l, err := Open(store.User{ID: "t", Name: "t", Root: root, Library: id}, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	if !l.attach() {
		t.Fatal("attach failed")
	}
	l.scan(-1)
	return l, l.root.Load()
}

func idAt(l *Library, rel string) meta.UUID {
	var id string
	l.db.QueryRow("SELECT id FROM tracks WHERE path = ?", rel).Scan(&id)
	u, _ := meta.ParseUUID(id)
	return u
}

func genre(g string) taglib.Edit { return taglib.Edit{Genre: taglib.Opt[string]{Set: true, V: &g}} }

// checkInSync fails unless every live track's indexed path holds that
// track's own file.
func checkInSync(t *testing.T, l *Library, r *Root) {
	t.Helper()
	rows, _ := l.db.Query("SELECT id, path FROM tracks WHERE path IS NOT NULL")
	defer rows.Close()
	for rows.Next() {
		var id, rel string
		rows.Scan(&id, &rel)
		f, err := r.Open(rel, unix.O_RDONLY, 0)
		if err != nil {
			t.Errorf("track %s indexed at %s, which is missing", id, rel)
			continue
		}
		buf := make([]byte, 64)
		n, _ := unix.Fgetxattr(int(f.Fd()), xattrName, buf)
		f.Close()
		if string(buf[:n]) != id {
			t.Errorf("track %s indexed at %s, which holds %s", id, rel, buf[:n])
		}
	}
}

// Beam: a refile after a tag edit landed on a path the index still gave to
// another track whose file had been moved outside the server. The index lost
// the edited track, and the next scan deleted the other one.
func TestEditRefileOntoStaleIndexedPath(t *testing.T) {
	l, r := testLibrary(t, "a.flac", "b.flac")
	a, b := idAt(l, "a.flac"), idAt(l, "b.flac")
	if _, err := l.Edit([]meta.UUID{b}, genre("x"), ""); err != nil {
		t.Fatal(err)
	}
	tb, _ := l.Track(b)
	filed := tb.RelativePath // where the profile files this song
	// Another client moves B on the share; this server hasn't scanned since.
	os.MkdirAll(filepath.Join(r.Path, "elsewhere"), 0o755)
	moved := filepath.Join(r.Path, "elsewhere/b.flac")
	if err := os.Rename(filepath.Join(r.Path, filed), moved); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(moved, old, old)
	if _, err := l.Edit([]meta.UUID{a}, genre("y"), ""); err != nil {
		t.Fatal(err)
	}
	ta, _ := l.Track(a)
	if ta.RelativePath == filed {
		t.Fatalf("A refiled onto %s, which the index still gives to B", filed)
	}
	l.scan(-1)
	if _, err := l.Track(b); err != nil {
		t.Fatalf("B was deleted, though its file is at elsewhere/b.flac")
	}
	if _, err := l.Track(a); err != nil {
		t.Fatalf("A was deleted")
	}
	checkInSync(t, l, r)
}

func TestScanForgetsOnlyOnSecondMiss(t *testing.T) {
	l, r := testLibrary(t, "a.flac")
	a := idAt(l, "a.flac")
	os.Rename(filepath.Join(r.Path, "a.flac"), filepath.Join(r.Path, ".away"))
	l.scan(-1)
	if _, err := l.Track(a); err != nil {
		t.Fatal("forgotten on the first miss")
	}
	os.Rename(filepath.Join(r.Path, ".away"), filepath.Join(r.Path, "a.flac")) // a glitch, over
	l.scan(-1)
	os.Remove(filepath.Join(r.Path, "a.flac"))
	l.scan(-1)
	if _, err := l.Track(a); err != nil {
		t.Fatal("a miss after the file came back counted as the second")
	}
	l.scan(-1)
	if _, err := l.Track(a); err == nil {
		t.Fatal("still indexed after two misses")
	}
}

// A batch naming a track whose file is gone answers not found and writes
// nothing; other failures don't read as a missing track.
func TestEditChecksEveryTrackFirst(t *testing.T) {
	l, r := testLibrary(t, "x/a.flac", "y/b.flac")
	a, b := idAt(l, "x/a.flac"), idAt(l, "y/b.flac")
	before, _ := os.ReadFile(filepath.Join(r.Path, "x/a.flac"))
	os.Remove(filepath.Join(r.Path, "y/b.flac"))
	if _, err := l.Edit([]meta.UUID{a, b}, genre("z"), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v, want ErrNotFound", err)
	}
	if after, err := os.ReadFile(filepath.Join(r.Path, "x/a.flac")); err != nil || !bytes.Equal(before, after) {
		t.Fatal("the first track was written though the batch failed")
	}
	if _, err := l.Edit([]meta.UUID{meta.NewUUID()}, genre("z"), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: %v, want ErrNotFound", err)
	}

	// A file that now holds another track: the index is behind.
	os.Rename(filepath.Join(r.Path, "x/a.flac"), filepath.Join(r.Path, "y/b.flac"))
	os.WriteFile(filepath.Join(r.Path, "x/a.flac"), before, 0o644)
	unix.Setxattr(filepath.Join(r.Path, "x/a.flac"), xattrName, []byte(b.String()), 0)
	if _, err := l.Edit([]meta.UUID{a}, genre("z"), ""); err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotFound) {
		t.Fatalf("another track's file: %v, want a failure that isn't not-found", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only folder below")
	}
	l2, r2 := testLibrary(t, "ro/a.flac")
	os.Chmod(filepath.Join(r2.Path, "ro"), 0o555)
	defer os.Chmod(filepath.Join(r2.Path, "ro"), 0o755)
	_, err := l2.Edit([]meta.UUID{idAt(l2, "ro/a.flac")}, genre("z"), "")
	if err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "create temporary copy") {
		t.Fatalf("unwritable folder: %v, want the failing step, not not-found", err)
	}
}
