package taglib

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
)

func opt[T any](v T) Opt[T] { return Opt[T]{Set: true, V: &v} }

// TestWriteRoundTrip writes the Mac editor's fields to one file of each
// container and reads them back through the scanner path.
func TestWriteRoundTrip(t *testing.T) {
	for _, name := range []string{"hires.flac", "tagged.mp3", "alac.m4a", "aac.m4a", "aiff.aiff", "wave.wav"} {
		path := filepath.Join(t.TempDir(), name)
		data, _ := os.ReadFile(filepath.Join("../testdata/audio", name))
		os.WriteFile(path, data, 0o644)
		ext := filepath.Ext(name)[1:]
		open := func(write bool) *File {
			mode := unix.O_RDONLY
			if write {
				mode = unix.O_RDWR
			}
			fd, _ := unix.Open(path, mode, 0)
			f, err := Open(fd, ext, write)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return f
		}
		f := open(true)
		f.Apply(Edit{
			Title: opt("New Title"), Artist: opt("Alpha ; Beta"), AlbumArtist: opt("Alpha"), Album: opt("Album"),
			Genre: opt("House"), SecondaryGenres: opt([]string{"Techno", "IDM"}), Year: opt(1999), TrackNumber: opt(7),
			DiscNumber: opt(2), DiscTotal: opt(3), Compilation: opt(true), Label: opt("Label"),
		}, f.Read(), ext == "flac", ", ")
		if err := f.Save(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f.Close()
		f = open(false)
		got := f.Read()
		f.Close()
		if got.Title != "New Title" || *got.Album != "Album" || *got.Year != 1999 || *got.TrackNumber != 7 || !got.Compilation {
			t.Errorf("%s: basic fields %+v", name, got)
		}
		// The file's existing list separator is kept, as MetadataWriter does.
		if !slices.Equal(meta.ValuesOfStored(deref(got.Artist)), []string{"Alpha", "Beta"}) || !got.ArtistIsList {
			t.Errorf("%s: artist %v list=%v", name, deref(got.Artist), got.ArtistIsList)
		}
		if deref(got.Genre) != "House" || !slices.Equal(got.SecondaryGenres, []string{"Techno", "IDM"}) {
			t.Errorf("%s: genres %v %v", name, deref(got.Genre), got.SecondaryGenres)
		}
		if deref(got.DiscNumber) != 2 || deref(got.DiscTotal) != 3 || deref(got.Label) != "Label" || deref(got.TagVersion) != 2 {
			t.Errorf("%s: disc %v/%v label %v version %v", name, deref(got.DiscNumber), deref(got.DiscTotal), deref(got.Label), deref(got.TagVersion))
		}
	}
}
