package library

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
	"flactastic/server/store"
	"flactastic/server/taglib"
)

// formatLibrary is a scanned library with three copies of each test format
// in one album folder.
func formatLibrary(t *testing.T) (*Library, []meta.UUID) {
	t.Helper()
	root, data := t.TempDir(), t.TempDir()
	old := time.Now().Add(-time.Hour)
	os.MkdirAll(filepath.Join(root, "al"), 0o755)
	var names []string
	for _, src := range []string{"cur-single.flac", "tagged.mp3", "aac.m4a", "alac.m4a", "wave.wav", "aiff.aiff"} {
		b, err := os.ReadFile("../testdata/audio/" + src)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			n := "al/" + string(rune('a'+i)) + "-" + src
			p := filepath.Join(root, n)
			os.WriteFile(p, b, 0o644)
			os.Chtimes(p, old, old)
			names = append(names, n)
		}
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
	var ids []meta.UUID
	for _, n := range names {
		u := idAt(l, n)
		if u == (meta.UUID{}) {
			t.Fatalf("%s not indexed", n)
		}
		ids = append(ids, u)
	}
	return l, ids
}

func cover(size int) []byte {
	b := make([]byte, size)
	rand.Read(b)
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0})
	return b
}

// A cover push from the Mac app: one batch edit setting artwork on a whole
// album, at sizes that overflow every format's padding, twice over.
func TestEditArtworkEveryFormat(t *testing.T) {
	l, ids := formatLibrary(t)
	for _, size := range []int{200 << 10, 3 << 20, 9 << 20} {
		art := cover(size)
		for pass := range 2 {
			out, _, err := l.Edit(ids, taglib.Edit{Artwork: taglib.Opt[[]byte]{Set: true, V: &art}}, "")
			if err != nil {
				t.Fatalf("%d KiB cover, pass %d: %v", size>>10, pass, err)
			}
			if len(out) != len(ids) {
				t.Fatalf("%d of %d tracks returned", len(out), len(ids))
			}
		}
	}
	checkInSync(t, l, l.root.Load())
}

// The cover lands in every format, on a local disk and on a network mount.
// WAV and AIFF lost it to TagLib's RIFF parser starting at the descriptor's
// offset, which the copy had left at EOF.
func TestEditArtworkReadsBack(t *testing.T) {
	for _, network := range []bool{false, true} {
		l, ids := formatLibrary(t)
		r := l.root.Load()
		r.network = network
		art := cover(9 << 20)
		for pass := range 2 {
			if _, _, err := l.Edit(ids, taglib.Edit{Artwork: taglib.Opt[[]byte]{Set: true, V: &art}}, ""); err != nil {
				t.Fatalf("network %v, pass %d: %v", network, pass, err)
			}
		}
		for _, id := range ids {
			tr, _ := l.Track(id)
			f, err := l.OpenTrack(tr)
			if err != nil {
				t.Fatal(err)
			}
			fd, _ := unix.Dup(int(f.Fd()))
			f.Close()
			tf, err := taglib.Open(fd, filepath.Ext(tr.RelativePath)[1:], false)
			if err != nil {
				t.Fatalf("%s: %v", tr.RelativePath, err)
			}
			got := tf.Picture()
			tf.Close()
			if len(got) != len(art) {
				t.Errorf("network %v, %s: cover is %d bytes, want %d", network, tr.RelativePath, len(got), len(art))
			}
		}
		checkInSync(t, l, r)
	}
}
