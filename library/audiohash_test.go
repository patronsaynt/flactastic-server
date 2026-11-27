package library

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
	"flactastic/server/taglib"
)

// hashes hashes rel beneath dir the way the hasher does.
func hashes(t *testing.T, dir, rel string) (content, audio string) {
	t.Helper()
	r, err := openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	content, audio, err = hashFile(context.Background(), r, rel, info.Size(), -1, -1, make([]byte, 4096))
	if err != nil {
		t.Fatal(err)
	}
	return content, audio
}

// retag rewrites a file's tags in place with taglib, as an edit does.
func retag(t *testing.T, file string, e taglib.Edit, flac bool) {
	t.Helper()
	fd, err := unix.Open(file, unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	tf, err := taglib.Open(fd, filepath.Ext(file)[1:], true)
	if err != nil {
		t.Fatal(err)
	}
	tf.Apply(e, tf.Read(), flac, "")
	err = tf.Save()
	tf.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeJPEG is n bytes that start like a JPEG.
func fakeJPEG(n int, fill byte) []byte {
	b := bytes.Repeat([]byte{fill}, n)
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0})
	return b
}

func TestAudioHashSurvivesRetagging(t *testing.T) {
	for _, name := range []string{"cur-single.flac", "tagged.mp3", "aac.m4a", "alac.m4a", "wave.wav", "aiff.aiff"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			copyTestFile(t, name, filepath.Join(dir, name))
			c0, a0 := hashes(t, dir, name)
			if a0 == "" {
				t.Fatal("no audio hash")
			}
			flac := filepath.Ext(name) == ".flac"
			g, title := "Retagged", "A New Title That Is Longer Than Before"
			small := fakeJPEG(2048, 1)
			retag(t, filepath.Join(dir, name), taglib.Edit{Genre: taglib.Opt[string]{Set: true, V: &g},
				Artwork: taglib.Opt[[]byte]{Set: true, V: &small}}, flac)
			c1, a1 := hashes(t, dir, name)
			// Different, far larger artwork moves the audio within the file.
			big := fakeJPEG(300<<10, 2)
			retag(t, filepath.Join(dir, name), taglib.Edit{Title: taglib.Opt[string]{Set: true, V: &title},
				Artwork: taglib.Opt[[]byte]{Set: true, V: &big}}, flac)
			c2, a2 := hashes(t, dir, name)
			if c0 == c1 || c1 == c2 {
				t.Fatal("a retag didn't change the file; the test proves nothing")
			}
			if a1 != a0 || a2 != a0 {
				t.Fatalf("audio hash changed with the tags: %s, %s, %s", a0[:12], a1[:12], a2[:12])
			}
		})
	}
}

func copyTestFile(t *testing.T, name, to string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../testdata/audio", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAudioHashTagEdges(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, parts ...[]byte) {
		os.WriteFile(filepath.Join(dir, name), bytes.Join(parts, nil), 0o644)
	}
	read := func(name string) []byte { b, _ := os.ReadFile(filepath.Join("../testdata/audio", name)); return b }
	// An ID3v2 tag with a footer: 10-byte header, 6 bytes of body, footer.
	id3 := append([]byte{'I', 'D', '3', 4, 0, 0x10, 0, 0, 0, 6}, make([]byte, 6)...)
	id3 = append(id3, '3', 'D', 'I', 4, 0, 0x10, 0, 0, 0, 6)

	flac := read("cur-single.flac")
	write("plain.flac", flac)
	write("id3.flac", id3, flac)
	_, a := hashes(t, dir, "plain.flac")
	if _, b := hashes(t, dir, "id3.flac"); a == "" || a != b {
		t.Errorf("FLAC behind an ID3v2 tag: %q vs %q", b, a)
	}

	mp3 := read("tagged.mp3")
	write("plain.mp3", mp3)
	// APEv2 with a header, then ID3v1, appended.
	ape := func(flags uint32) []byte {
		b := make([]byte, 32)
		copy(b, "APETAGEX")
		binary.LittleEndian.PutUint32(b[8:], 2000)
		binary.LittleEndian.PutUint32(b[12:], 32+16) // items and footer
		binary.LittleEndian.PutUint32(b[20:], flags)
		return b
	}
	v1 := append([]byte("TAG"), make([]byte, 125)...)
	write("tails.mp3", mp3, ape(1<<31|1<<29), make([]byte, 16), ape(1<<31), v1)
	_, a = hashes(t, dir, "plain.mp3")
	if _, b := hashes(t, dir, "tails.mp3"); a == "" || a != b {
		t.Errorf("MP3 with APEv2 and ID3v1 appended: %q vs %q", b, a)
	}

	// An APEv2 footer claiming less than its own size must not hang the hasher.
	write("badape.mp3", mp3, []byte("APETAGEX"), make([]byte, 24))
	if _, b := hashes(t, dir, "badape.mp3"); b == "" {
		t.Error("an MP3 with a broken APE footer lost its audio hash")
	}
	write("junk.flac", []byte("this is not audio at all, not even close"))
	write("junk.mp4", []byte("\x00\x00\x00\x10ftypM4A \x00\x00\x00\x00\x00\x00\x99\x99mdat"))
	for _, n := range []string{"junk.flac", "junk.mp4"} {
		if _, b := hashes(t, dir, n); b != "" {
			t.Errorf("%s: audio hash %q, want none", n, b)
		}
	}
}

// The hasher publishes audioHash, fills it in for tracks hashed before it
// existed without touching contentHash, and keeps it through a tag edit.
func TestAudioHashPublishedAndBackfilled(t *testing.T) {
	l, r := testLibrary(t, "al/a.flac", "al/b.mp3")
	mp3 := filepath.Join(r.Path, "al/b.mp3")
	copyTestFile(t, "tagged.mp3", mp3)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(mp3, old, old)
	l.scan(-1)
	drain := func() {
		t.Helper()
		for i := 0; ; i++ {
			n, failed := l.hashBatch(context.Background(), r, make([]byte, 4096))
			if n == 0 {
				return
			}
			if failed || i > 10 {
				t.Fatalf("hasher not settling: %d jobs, failed %v", n, failed)
			}
		}
	}
	drain()
	a := idAt(l, "al/a.flac")
	ta, _ := l.Track(a)
	if ta.ContentHash == nil || ta.AudioHash == nil {
		t.Fatalf("hashes not published: %v %v", ta.ContentHash, ta.AudioHash)
	}

	// A library from before audioHash: contentHash set, audio not.
	l.db.Exec("UPDATE tracks SET audio = NULL, data = json_remove(data, '$.audioHash')")
	seq := l.Seq()
	drain()
	tb, _ := l.Track(a)
	if tb.AudioHash == nil || *tb.AudioHash != *ta.AudioHash || *tb.ContentHash != *ta.ContentHash {
		t.Fatalf("backfill: audio %v content %v, want %s %s", tb.AudioHash, tb.ContentHash, *ta.AudioHash, *ta.ContentHash)
	}
	var trackSeq int64
	l.db.QueryRow("SELECT seq FROM tracks WHERE id = ?", a.String()).Scan(&trackSeq)
	if trackSeq <= seq {
		t.Fatal("backfill didn't publish a new seq")
	}

	if _, _, err := l.Edit([]meta.UUID{a}, genre("Changed"), ""); err != nil {
		t.Fatal(err)
	}
	drain()
	tc, _ := l.Track(a)
	if tc.ContentHash == nil || *tc.ContentHash == *ta.ContentHash {
		t.Fatal("contentHash didn't follow the edit")
	}
	if tc.AudioHash == nil || *tc.AudioHash != *ta.AudioHash {
		t.Fatalf("audioHash changed with a tag edit: %v", tc.AudioHash)
	}
}
