package main

import (
	"fmt"
	"strings"
	"testing"

	"flactastic/server/library"
	"flactastic/server/meta"
)

func track(title, artist, album string, year, n int) library.Track {
	t := library.Track{ID: meta.NewUUID(), Title: title, Artist: &artist, Album: &album, TrackNumber: &n, Format: meta.FLAC}
	if year > 0 {
		t.Year = &year
	}
	return t
}

func titles(ts []library.Track) (out []string) {
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return out
}

func TestLibrarySortAndFilter(t *testing.T) {
	tracks := []library.Track{
		track("b2", "Björk", "Début", 1993, 2), track("none", "Zedd", "Stay", 0, 1),
		track("b1", "Björk", "Début", 1993, 1), track("old", "Simon", "Bookends", 1968, 1),
	}
	cases := []struct {
		key     string
		reverse bool
		want    string
	}{
		{"", false, "b1 b2 old none"}, // album order, filed under the artist without an album artist
		{"year", false, "old b1 b2 none"},
		{"year", true, "b1 b2 old none"}, // no year stays last, ties stay in album order
		{"title", true, "old none b2 b1"},
	}
	for _, c := range cases {
		list := append([]library.Track{}, tracks...)
		sortTracks(list, c.key, c.reverse)
		if got := titles(list); strings.Join(got, " ") != c.want {
			t.Errorf("sort %q reverse=%v: got %v want %s", c.key, c.reverse, got, c.want)
		}
	}
	if got := titles(filterTracks(opts{artist: "bjork"}, tracks)); strings.Join(got, " ") != "b2 b1" {
		t.Errorf("artist filter ignoring accents: %v", got)
	}
	if got := filterTracks(opts{format: "MP3"}, tracks); len(got) != 0 {
		t.Errorf("format filter: %v", titles(got))
	}
}

func BenchmarkSortTracks100k(b *testing.B) {
	var tracks []library.Track
	for i := 0; i < 100000; i++ {
		tracks = append(tracks, track(fmt.Sprintf("Track %d", i), fmt.Sprintf("Artist %d", i/100), fmt.Sprintf("Album %d", i/10), 1990+i%30, i%10+1))
	}
	for b.Loop() {
		list := append([]library.Track{}, tracks...)
		sortTracks(list, "", false)
	}
}
