package library

import (
	"bytes"
	"errors"
	"io"
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
	if _, _, err := l.Edit([]meta.UUID{b}, genre("x"), ""); err != nil {
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
	if _, _, err := l.Edit([]meta.UUID{a}, genre("y"), ""); err != nil {
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
	if _, _, err := l.Edit([]meta.UUID{a, b}, genre("z"), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v, want ErrNotFound", err)
	}
	if after, err := os.ReadFile(filepath.Join(r.Path, "x/a.flac")); err != nil || !bytes.Equal(before, after) {
		t.Fatal("the first track was written though the batch failed")
	}
	if _, _, err := l.Edit([]meta.UUID{meta.NewUUID()}, genre("z"), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: %v, want ErrNotFound", err)
	}

	// A file that now holds another track: the index is behind.
	os.Rename(filepath.Join(r.Path, "x/a.flac"), filepath.Join(r.Path, "y/b.flac"))
	os.WriteFile(filepath.Join(r.Path, "x/a.flac"), before, 0o644)
	unix.Setxattr(filepath.Join(r.Path, "x/a.flac"), xattrName, []byte(b.String()), 0)
	if _, _, err := l.Edit([]meta.UUID{a}, genre("z"), ""); !errors.Is(err, ErrIndexStale) || errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotFound) {
		t.Fatalf("another track's file: %v, want a failure that isn't not-found", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only folder below")
	}
	l2, r2 := testLibrary(t, "ro/a.flac")
	os.Chmod(filepath.Join(r2.Path, "ro"), 0o555)
	defer os.Chmod(filepath.Join(r2.Path, "ro"), 0o755)
	_, _, err := l2.Edit([]meta.UUID{idAt(l2, "ro/a.flac")}, genre("z"), "")
	if !errors.Is(err, ErrReadOnly) || errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "create temporary copy") {
		t.Fatalf("unwritable folder: %v, want the failing step, not not-found", err)
	}
}

// On a network mount an edit must not replace the file while a stream has
// it open: SMB deletes the original when that fails.
func TestEditOnNetworkMountWhileStreaming(t *testing.T) {
	l, r := testLibrary(t, "al/a.flac")
	r.network = true
	a := idAt(l, "al/a.flac")
	before, _ := os.ReadFile(filepath.Join(r.Path, "al/a.flac"))
	stream, err := os.Open(filepath.Join(r.Path, "al/a.flac"))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	out, _, err := l.Edit([]meta.UUID{a}, genre("Bass"), "")
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Genre == nil || *out[0].Genre != "Bass" {
		t.Fatalf("genre not written: %v", out[0].Genre)
	}
	if b, _ := io.ReadAll(stream); !bytes.Equal(b, before) {
		t.Error("the stream didn't keep the old bytes")
	}
	entries, _ := os.ReadDir(filepath.Join(r.Path, filepath.Dir(out[0].RelativePath)))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".flactastic-") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	checkInSync(t, l, r)
}

// Organize apply on a library whose index trails the disk (an SMB mount
// rescans every 10 minutes): a file the plan moves has gone. Nothing may
// move; the answer is planStale, and a scan brings the index up to date.
func TestOrganizeApplyWithMissingSource(t *testing.T) {
	l, r := testLibrary(t, "in/a.flac", "in/b.mp3")
	copyTestFile(t, "tagged.mp3", filepath.Join(r.Path, "in/b.mp3"))
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(r.Path, "in/b.mp3"), old, old)
	l.scan(-1)
	hash, ops, err := l.OrganizePlan(nil)
	if err != nil {
		t.Fatal(err)
	}
	moving := 0
	for _, o := range ops {
		if o.Status != "unchanged" {
			moving++
		}
	}
	if moving != 2 {
		t.Fatalf("%d moves planned, want 2", moving)
	}
	os.Remove(filepath.Join(r.Path, "in/b.mp3")) // gone, the index not told
	moved, _, err := l.OrganizeApply(nil, hash)
	if !errors.Is(err, ErrPlanStale) || moved != 0 {
		t.Fatalf("apply: moved %d, %v; want nothing moved and ErrPlanStale", moved, err)
	}
	if _, err := os.Stat(filepath.Join(r.Path, "in/a.flac")); err != nil {
		t.Fatal("a.flac moved though the apply was refused")
	}
	l.scan(-1)
	l.scan(-1) // a missing file is forgotten on the second look
	hash, _, _ = l.OrganizePlan(nil)
	if moved, _, err := l.OrganizeApply(nil, hash); err != nil || moved != 1 {
		t.Fatalf("after the scan: moved %d, %v", moved, err)
	}
	checkInSync(t, l, r)
}

// A copy of a file gets its own trackID, and the file must carry it, or
// every later check takes the copy for the original.
func TestCopyGetsItsOwnIDOnDisk(t *testing.T) {
	l, r := testLibrary(t, "a.flac")
	a := idAt(l, "a.flac")
	src, _ := os.ReadFile(filepath.Join(r.Path, "a.flac"))
	os.WriteFile(filepath.Join(r.Path, "b.flac"), src, 0o644)
	unix.Setxattr(filepath.Join(r.Path, "b.flac"), xattrName, []byte(a.String()), 0) // cp --preserve=xattr
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(r.Path, "b.flac"), old, old)
	l.scan(-1)
	b := idAt(l, "b.flac")
	if b == a || b == (meta.UUID{}) {
		t.Fatalf("copy: id %v, original %v", b, a)
	}
	checkInSync(t, l, r)
}

func TestEditErrKinds(t *testing.T) {
	tr := Track{RelativePath: "a.flac"}
	for _, c := range []struct {
		err  error
		want error
	}{
		{&os.PathError{Op: "write", Path: "x", Err: unix.ENOSPC}, ErrNoSpace},
		{&os.PathError{Op: "write", Path: "x", Err: unix.EDQUOT}, ErrNoSpace},
		{&os.PathError{Op: "create", Path: "x", Err: unix.EROFS}, ErrReadOnly},
		{&os.PathError{Op: "rename", Path: "x", Err: unix.EPERM}, ErrReadOnly},
		{taglib.ErrSave, ErrTagWrite},
		{taglib.ErrOpen, ErrTagWrite},
	} {
		if err := editErr(tr, "step", c.err); !errors.Is(err, c.want) {
			t.Errorf("%v: %v, want %v", c.err, err, c.want)
		}
	}
	if err := editErr(tr, "replace original", &os.PathError{Op: "rename", Path: "x", Err: unix.ENOENT}); errors.Is(err, os.ErrNotExist) {
		t.Error("ENOENT passed on")
	}
}

// A batch where one track can't be written writes the others and names
// the one that failed; when none is written, the batch fails.
func TestEditBatchPartial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only folder")
	}
	l, r := testLibrary(t, "ro/a.flac", "rw/b.flac")
	a, b := idAt(l, "ro/a.flac"), idAt(l, "rw/b.flac")
	os.Chmod(filepath.Join(r.Path, "ro"), 0o555)
	defer os.Chmod(filepath.Join(r.Path, "ro"), 0o755)
	out, failed, err := l.Edit([]meta.UUID{a, b}, genre("Dub"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != b || out[0].Genre == nil || *out[0].Genre != "Dub" {
		t.Fatalf("written: %+v", out)
	}
	if len(failed) != 1 || failed[0].ID != a || !errors.Is(failed[0].Err, ErrReadOnly) {
		t.Fatalf("failed: %+v", failed)
	}
	if _, _, err := l.Edit([]meta.UUID{a}, genre("Dub"), ""); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("nothing written: %v", err)
	}
}
