package library

import (
	"database/sql"
	"encoding/json"
	"testing"

	"flactastic/server/meta"
	"flactastic/server/store"
)

func TestSearch(t *testing.T) {
	dir := t.TempDir()
	u := store.User{ID: "s", Name: "s", Root: dir}
	l, err := Open(u, dir)
	if err != nil {
		t.Fatal(err)
	}
	add := func(title, artist, album string) {
		tr := Track{ID: meta.NewUUID(), Title: title, Artist: &artist, Album: &album, RelativePath: title + ".flac", SecondaryGenres: []string{}}
		data, _ := json.Marshal(tr)
		// No search column, as rows indexed before it existed.
		if err := l.write(func(tx *sql.Tx, seq int64) error {
			_, err := tx.Exec("INSERT INTO tracks(id, path, seq, data) VALUES(?,?,?,?)", tr.ID.String(), tr.RelativePath, seq, data)
			return err
		}, KindTracks); err != nil {
			t.Fatal(err)
		}
	}
	add("Jóga", "Björk", "Homogenic")
	add("Army of Me", "Björk", "Post")
	add("Tone", "Daft", "Punk Album")
	l.Close()
	if l, err = Open(u, dir); err != nil { // fills search
		t.Fatal(err)
	}
	defer l.Close()
	for q, want := range map[string]int{"bjork": 2, "  BJÖRK ": 2, "joga": 1, "daft punk": 0, "daft\npunk": 0, "punk": 1, "": 0, "zzz": 0} {
		got, err := l.Search(q, 50)
		if err != nil || len(got) != want {
			t.Errorf("Search(%q) = %d tracks, %v; want %d", q, len(got), err, want)
		}
	}
	if got, _ := l.Search("bjork", 1); len(got) != 1 {
		t.Errorf("limit 1 gave %d", len(got))
	}
}

func TestAlbumsFollowTrackChanges(t *testing.T) {
	l := benchLibrary(t, 20)
	before, _ := l.Albums()
	var id string
	l.db.QueryRow("SELECT id FROM tracks LIMIT 1").Scan(&id)
	if err := l.write(func(tx *sql.Tx, seq int64) error {
		_, err := tx.Exec("UPDATE tracks SET seq = ?, data = json_set(data, '$.album', 'Moved Album') WHERE id = ?", seq, id)
		return err
	}, KindTracks); err != nil {
		t.Fatal(err)
	}
	after, _ := l.Albums()
	if len(after) != len(before)+1 || after[len(after)-1].Name != "Moved Album" {
		t.Fatalf("albums didn't follow the change: %d then %d", len(before), len(after))
	}
	if again, _ := l.Albums(); &again[0] != &after[0] {
		t.Fatal("unchanged library recomputed albums")
	}
}
