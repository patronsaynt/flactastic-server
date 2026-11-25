package library

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
