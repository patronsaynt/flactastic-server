package taglib

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

// TestMacScannerParity reads every fixture and compares the tag fields with
// what the Mac's LibraryScanner produced for the same file.
func TestMacScannerParity(t *testing.T) {
	raw, err := os.ReadFile("../testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Files []map[string]any }
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, want := range v.Files {
		name := want["file"].(string)
		fd, err := unix.Open(filepath.Join("../testdata/audio", name), unix.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f, err := Open(fd, filepath.Ext(name)[1:], false)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		tags, pic := f.Read(), f.Picture()
		f.Close()
		title := tags.Title
		if title == "" {
			title = name[:len(name)-len(filepath.Ext(name))]
		}
		var art any
		if pic != nil {
			sum := sha256.Sum256(pic)
			art = hex.EncodeToString(sum[:])
		}
		got := map[string]any{
			"title": title, "artist": str(tags.Artist), "albumArtist": str(tags.AlbumArtist), "album": str(tags.Album),
			"trackNumber": num(tags.TrackNumber), "genre": str(tags.Genre), "secondaryGenres": list(tags.SecondaryGenres),
			"year": num(tags.Year), "isCompilation": tags.Compilation, "isMixCompilation": tags.MixCompilation,
			"label": str(tags.Label), "discNumber": num(tags.DiscNumber), "discTotal": num(tags.DiscTotal),
			"tagVersion": num(tags.TagVersion), "artistIsList": tags.ArtistIsList, "albumArtistIsList": tags.AlbumArtistIsList,
			"hasArtwork": pic != nil, "artworkSHA256": art,
		}
		for k, g := range got {
			if !reflect.DeepEqual(g, want[k]) {
				t.Errorf("%s: %s = %#v, Mac read %#v", name, k, g, want[k])
			}
		}
	}
}

func str(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func num(p *int) any {
	if p == nil {
		return nil
	}
	return float64(*p)
}

func list(xs []string) []any {
	out := []any{}
	for _, x := range xs {
		out = append(out, x)
	}
	return out
}
