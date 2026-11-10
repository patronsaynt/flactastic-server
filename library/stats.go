package library

import (
	"fmt"
	"sort"
	"time"

	"flactastic/server/meta"
)

// Stats ported from the Mac's ListeningStore metrics (protocol §15).

type Ranked struct {
	Name    string  `json:"name"`
	Plays   int     `json:"plays"`
	Minutes float64 `json:"minutes"`
}

type AlbumRank struct {
	AlbumID string  `json:"albumID"`
	Album   string  `json:"album"`
	Artist  string  `json:"artist"`
	Plays   int     `json:"plays"`
	Minutes float64 `json:"minutes"`
}

type OnThisDay struct {
	AlbumRank
	Headline string    `json:"headline"`
	Date     meta.Time `json:"date"`
}

type Listening struct {
	TotalSeconds float64     `json:"totalSeconds"`
	TracksPlayed int         `json:"tracksPlayed"`
	AlbumsPlayed int         `json:"albumsPlayed"`
	Sessions     int         `json:"sessions"`
	StreakDays   int         `json:"streakDays"`
	TopArtists   []Ranked    `json:"topArtists"`
	TopAlbums    []AlbumRank `json:"topAlbums"`
	TopGenre     *struct {
		Name  string  `json:"name"`
		Share float64 `json:"share"`
	} `json:"topGenre,omitempty"`
	OnThisDay *OnThisDay `json:"onThisDay,omitempty"`
}

const sessionGap = 30 * time.Minute

func (l *Library) Listening(loc *time.Location, since *time.Time, limit int, now time.Time) (Listening, error) {
	all, err := l.events()
	if err != nil {
		return Listening{}, err
	}
	var ev []PlayEvent
	for _, e := range all {
		if since == nil || !e.Date.Before(*since) {
			ev = append(ev, e)
		}
	}
	var s Listening
	albums := map[string]bool{}
	genres := map[string]int{}
	counted := 0
	for i, e := range ev {
		s.TotalSeconds += e.SecondsListened
		if i == 0 || e.Date.Sub(ev[i-1].Date.Time) > sessionGap {
			s.Sessions++
		}
		if !e.Counted {
			continue
		}
		counted++
		s.TracksPlayed++
		if e.AlbumID != nil {
			albums[*e.AlbumID] = true
		}
		seen := map[string]bool{}
		for _, g := range append([]string{or(e.Genre, "")}, e.SecondaryGenres...) {
			if g != "" && !seen[meta.Lower(g)] {
				seen[meta.Lower(g)] = true
				genres[g]++
			}
		}
	}
	s.AlbumsPlayed = len(albums)
	var top string
	for g, n := range genres {
		if top == "" || n > genres[top] || (n == genres[top] && g < top) {
			top = g
		}
	}
	if top != "" {
		s.TopGenre = &struct {
			Name  string  `json:"name"`
			Share float64 `json:"share"`
		}{top, float64(genres[top]) / float64(counted)}
	}
	s.StreakDays = streak(all, loc, now)
	s.TopArtists = topArtists(ev, limit)
	s.TopAlbums = rankAlbums(ev, func(a AlbumRank) bool { return a.Minutes > 0 }, limit)
	available := map[string]bool{}
	if tracks, err := l.allTracks(); err == nil {
		for _, a := range groupAlbums(tracks) {
			available[a.ID] = true
		}
	}
	s.OnThisDay = onThisDay(all, loc, now, available)
	return s, nil
}

func day(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// addYears moves a calendar day by whole years the way Foundation does:
// 29 February becomes 28 February rather than 1 March.
func addYears(d time.Time, years int) time.Time {
	out := d.AddDate(years, 0, 0)
	if out.Day() != d.Day() {
		out = out.AddDate(0, 0, -out.Day())
	}
	return out
}

func streak(ev []PlayEvent, loc *time.Location, now time.Time) int {
	days := map[time.Time]bool{}
	for _, e := range ev {
		days[day(e.Date.Time, loc)] = true
	}
	cursor := day(now, loc)
	if !days[cursor] {
		cursor = cursor.AddDate(0, 0, -1)
	}
	n := 0
	for days[cursor] {
		n++
		cursor = cursor.AddDate(0, 0, -1)
	}
	return n
}

func topArtists(ev []PlayEvent, limit int) []Ranked {
	by := map[string]*Ranked{}
	for _, e := range ev {
		if e.Artist == nil || *e.Artist == "" {
			continue
		}
		r := by[*e.Artist]
		if r == nil {
			r = &Ranked{Name: *e.Artist}
			by[r.Name] = r
		}
		r.Minutes += e.SecondsListened / 60
		if e.Counted {
			r.Plays++
		}
	}
	out := []Ranked{}
	for _, r := range by {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Minutes != out[j].Minutes {
			return out[i].Minutes > out[j].Minutes
		}
		return meta.StandardLess(out[i].Name, out[j].Name)
	})
	return out[:min(limit, len(out))]
}

func rankAlbums(ev []PlayEvent, keep func(AlbumRank) bool, limit int) []AlbumRank {
	by := map[string]*AlbumRank{}
	for _, e := range ev {
		if e.AlbumID == nil {
			continue
		}
		r := by[*e.AlbumID]
		if r == nil {
			r = &AlbumRank{AlbumID: *e.AlbumID, Album: or(e.Album, "Unknown Album"), Artist: or(e.Artist, "")}
			by[r.AlbumID] = r
		}
		if e.Counted {
			r.Plays++
		}
		r.Minutes += e.SecondsListened / 60
	}
	out := []AlbumRank{}
	for _, r := range by {
		if keep(*r) {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Minutes != out[j].Minutes {
			return out[i].Minutes > out[j].Minutes
		}
		if out[i].Plays != out[j].Plays {
			return out[i].Plays > out[j].Plays
		}
		return meta.StandardLess(out[i].Album, out[j].Album)
	})
	return out[:min(limit, len(out))]
}

func onThisDay(ev []PlayEvent, loc *time.Location, now time.Time, available map[string]bool) *OnThisDay {
	if len(ev) == 0 {
		return nil
	}
	today, oldest := day(now, loc), day(ev[0].Date.Time, loc)
	pick := func(days []time.Time, headline string) *OnThisDay {
		set := map[time.Time]bool{}
		for _, d := range days {
			set[d] = true
		}
		var best *OnThisDay
		by := map[string]*OnThisDay{}
		for _, e := range ev {
			if e.AlbumID == nil || !set[day(e.Date.Time, loc)] {
				continue
			}
			p := by[*e.AlbumID]
			if p == nil {
				p = &OnThisDay{AlbumRank: AlbumRank{AlbumID: *e.AlbumID, Album: or(e.Album, "Unknown Album"),
					Artist: or(e.Artist, "")}, Headline: headline, Date: e.Date}
				by[p.AlbumID] = p
			}
			if e.Counted {
				p.Plays++
			}
			p.Minutes += e.SecondsListened / 60
			if e.Date.Before(p.Date.Time) {
				p.Date = e.Date
			}
		}
		for _, p := range by {
			if p.Plays == 0 || !available[p.AlbumID] {
				continue
			}
			if best == nil || p.Plays > best.Plays || (p.Plays == best.Plays && (p.Minutes > best.Minutes ||
				(p.Minutes == best.Minutes && meta.StandardLess(p.Album, best.Album)))) {
				best = p
			}
		}
		return best
	}
	for years := 1; !addYears(today, -years).Before(oldest); years++ {
		headline := "A year ago today you played"
		if years > 1 {
			headline = fmt.Sprintf("%d years ago today you played", years)
		}
		if hit := pick([]time.Time{addYears(today, -years)}, headline); hit != nil {
			return hit
		}
	}
	lastYear := addYears(today, -1)
	var week []time.Time
	for d := -3; d <= 3; d++ {
		week = append(week, lastYear.AddDate(0, 0, d))
	}
	return pick(week, "This week last year you played")
}
