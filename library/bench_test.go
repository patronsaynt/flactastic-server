package library

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"flactastic/server/meta"
	"flactastic/server/store"
)

// benchLibrary is an index of n synthetic tracks, filed as the default
// profile would file them, with no music folder behind it.
func benchLibrary(b testing.TB, n int) *Library {
	dir := b.TempDir()
	l, err := Open(store.User{ID: "bench", Name: "bench", Root: dir}, dir)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(l.Close)
	err = l.write(func(tx *sql.Tx, seq int64) error {
		for i := 0; i < n; i++ {
			artist, album := fmt.Sprintf("Artist %d", i/100), fmt.Sprintf("Album %d", i/10)
			num, year := i%10+1, 1990+i%30
			t := Track{ID: meta.NewUUID(), Title: fmt.Sprintf("Track %d", i), Artist: &artist, Album: &album,
				TrackNumber: &num, Year: &year, Format: meta.FLAC, SecondaryGenres: []string{},
				DateAdded: meta.Time{Time: time.Unix(int64(i), 0)}}
			t.RelativePath = fmt.Sprintf("%s/%s (%d)/%02d - %s.flac", artist, album, year, num, t.Title)
			hash := fmt.Sprintf("%064x", i)
			t.ContentHash = &hash
			data, _ := json.Marshal(t)
			if _, err := tx.Exec("INSERT INTO tracks(id, path, seq, size, mtime, inode, ctime, hash, search, data) VALUES(?,?,?,?,?,?,?,?,?,?)",
				t.ID.String(), t.RelativePath, seq, 1000, 1, i, 1, hash, searchText(t), data); err != nil {
				return err
			}
		}
		return nil
	}, KindTracks)
	if err != nil {
		b.Fatal(err)
	}
	return l
}

func BenchmarkOrganizePlan100k(b *testing.B) {
	l := benchLibrary(b, 100000)
	for b.Loop() {
		if _, _, err := l.OrganizePlan(nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAlbums100k(b *testing.B) {
	l := benchLibrary(b, 100000)
	for b.Loop() {
		l.Albums()
	}
}

func BenchmarkSearch100k(b *testing.B) {
	l := benchLibrary(b, 100000)
	for b.Loop() {
		l.Search("track 99999", 50)
	}
}

func BenchmarkTrackFeed100k(b *testing.B) {
	l := benchLibrary(b, 100000)
	for b.Loop() {
		for since, more := int64(0), true; more; {
			var err error
			_, _, since, more, err = l.TracksSince(since, 5000)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkHashFile40M measures the hasher's single pass over a 40 MB FLAC,
// computing contentHash and audioHash together.
func BenchmarkHashFile40M(b *testing.B) {
	dir := b.TempDir()
	flac, err := os.ReadFile("../testdata/audio/cur-single.flac")
	if err != nil {
		b.Fatal(err)
	}
	noise := make([]byte, 40<<20)
	rand.Read(noise)
	os.WriteFile(filepath.Join(dir, "big.flac"), append(flac, noise...), 0o644)
	r, err := openRoot(dir)
	if err != nil {
		b.Fatal(err)
	}
	size := int64(len(flac) + len(noise))
	buf := make([]byte, 1<<20)
	b.SetBytes(size)
	for b.Loop() {
		if _, audio, err := hashFile(context.Background(), r, "big.flac", size, -1, -1, buf); err != nil || audio == "" {
			b.Fatal(err)
		}
	}
}
