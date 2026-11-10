package meta

import (
	"fmt"
	"testing"
)

func BenchmarkPlanOrganize100k(b *testing.B) {
	var tracks []OrganizerTrack
	var credits []Credit
	for i := 0; i < 100000; i++ {
		artist, album, title := fmt.Sprintf("Artist %d", i/100), fmt.Sprintf("Album %d", i/10), fmt.Sprintf("Track %d", i)
		n, y := i%20+1, 1990+i%30
		tracks = append(tracks, OrganizerTrack{ID: NewUUID(), Path: fmt.Sprintf("in/%d.flac", i), Title: title,
			Artist: &artist, Album: &album, TrackNumber: &n, Year: &y, Format: FLAC})
		credits = append(credits, Credit{Artist: &artist})
	}
	r := NewArtistResolver(credits, nil)
	b.ResetTimer()
	for b.Loop() {
		PlanOrganize(tracks, DefaultProfile, r)
	}
}
