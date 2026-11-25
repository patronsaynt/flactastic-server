package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"flactastic/server/library"
	"flactastic/server/meta"
	"flactastic/server/store"
)

// The library command shows one user's library the way the apps do: albums
// grouped as the Mac groups them, credits rather than file names.

var sortKeys = map[string][]string{
	"tracks":    {"album", "title", "artist", "year", "added", "duration", "size", "format", "path"},
	"albums":    {"name", "artist", "year", "tracks", "duration", "added"},
	"artists":   {"name", "albums", "tracks", "duration"},
	"playlists": {"name", "tracks", "created"},
	"playlist":  {"position", "title", "artist", "album", "year", "added", "duration", "size", "format", "path"},
}

func libraryCmd(o opts, s *store.Store) error {
	view, who := "", ""
	if len(o.args) > 1 {
		view = o.args[1]
	}
	if len(o.args) > 2 {
		who = o.args[2]
	}
	if _, ok := sortKeys[view]; !ok && view != "stats" {
		return fmt.Errorf("unknown command %q; see --help", strings.Join(o.args, " "))
	}
	if who == "" || (view == "playlist") != (len(o.args) == 4) || len(o.args) > 4 {
		return fmt.Errorf("usage: flactastic-server library %s NAME%s", view, map[bool]string{true: " PLAYLIST"}[view == "playlist"])
	}
	if view == "stats" && o.sort != "" {
		return fmt.Errorf("library stats doesn't sort")
	}
	if keys := sortKeys[view]; o.sort != "" && !slices.Contains(keys, o.sort) {
		return fmt.Errorf("library %s sorts by %s", view, strings.Join(keys, ", "))
	}
	u, err := s.UserByName(who)
	if err != nil {
		return fmt.Errorf("no user %q", who)
	}
	l, err := library.Open(u, o.data)
	if err != nil {
		return err
	}
	defer l.Close()

	var all []library.Track
	if err := l.Tracks(func(b []byte) error {
		var t library.Track
		err := json.Unmarshal(b, &t)
		all = append(all, t)
		return err
	}); err != nil {
		return err
	}
	tracks := filterTracks(o, all)
	if len(tracks) == 0 && (view == "tracks" || view == "albums" || view == "artists") {
		if len(all) == 0 {
			fmt.Printf("%s's library has no tracks yet. Is the server running? It reads the music folder after it starts.\n", u.Name)
		} else {
			fmt.Println("No tracks match.")
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	switch view {
	case "tracks":
		sortTracks(tracks, o.sort, o.reverse)
		printTracks(w, tracks, nil)
	case "albums":
		albums := library.GroupAlbums(tracks)
		byID := trackMap(tracks)
		sortAlbums(albums, byID, o.sort, o.reverse)
		fmt.Fprintln(w, "ALBUM\tARTIST\tYEAR\tTRACKS\tLENGTH")
		for _, a := range albums {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", a.Name, orUnknown(a.Artist), year(a.Year), len(a.TrackIDs), length(albumLength(a, byID)))
		}
	case "artists":
		printArtists(w, library.GroupAlbums(tracks), trackMap(tracks), o.sort, o.reverse)
	case "playlists", "playlist":
		return playlists(w, l, o, view, trackMap(all), tracks)
	case "stats":
		return stats(w, u, l, tracks)
	}
	return nil
}

// filterTracks keeps tracks matching --artist, --album and --format. Text
// matches ignore case and diacritics, as the apps' search does.
func filterTracks(o opts, all []library.Track) []library.Track {
	artist, album := meta.ArtistKey(o.artist), meta.ArtistKey(o.album)
	has := func(field *string, needle string) bool {
		return field != nil && strings.Contains(meta.ArtistKey(*field), needle)
	}
	var out []library.Track
	for _, t := range all {
		if artist != "" && !has(t.Artist, artist) && !has(t.AlbumArtist, artist) {
			continue
		}
		if album != "" && !has(t.Album, album) {
			continue
		}
		if o.format != "" && !strings.EqualFold(string(t.Format), o.format) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func trackMap(tracks []library.Track) map[meta.UUID]library.Track {
	m := make(map[meta.UUID]library.Track, len(tracks))
	for _, t := range tracks {
		m[t.ID] = t
	}
	return m
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func orUnknown(p *string) string {
	if p == nil {
		return "Unknown Artist"
	}
	return *p
}

func num(p *int) int {
	if p == nil {
		return 1 << 30
	}
	return *p
}

func year(p *int) string {
	if p == nil {
		return ""
	}
	return fmt.Sprint(*p)
}

func length(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	if d >= time.Hour {
		return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
	return clock(d)
}

// sortBy sorts stably by key, which reports -1, 0 or 1 and flips for
// --reverse, then by tie, which doesn't. Items blank for the key go last
// either way.
func sortBy[T any](list []T, reverse bool, key func(a, b T) int, blank func(T) bool, tie func(a, b T) int) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if blank != nil && blank(a) != blank(b) {
			return blank(b)
		}
		c := key(a, b)
		if reverse {
			c = -c
		}
		if c == 0 {
			c = tie(a, b)
		}
		return c < 0
	})
}

// cmpText orders text the way the apps' lists do, numbers numerically.
var cmpText = meta.StandardCompare

// filedUnder is the album artist, else the track artist, as the organizer's
// {albumArtist} token falls back.
func filedUnder(t library.Track) string {
	if t.AlbumArtist != nil {
		return *t.AlbumArtist
	}
	return orUnknown(t.Artist)
}

// albumOrder is the apps' album view order: album artist, album, disc, track.
func albumOrder(a, b library.Track) int {
	if c := cmpText(filedUnder(a), filedUnder(b)); c != 0 {
		return c
	}
	if c := cmpText(str(a.Album), str(b.Album)); c != 0 {
		return c
	}
	if c := cmp.Compare(num(a.DiscNumber), num(b.DiscNumber)); c != 0 {
		return c
	}
	if c := cmp.Compare(num(a.TrackNumber), num(b.TrackNumber)); c != 0 {
		return c
	}
	return cmpText(a.Title, b.Title)
}

var trackKeys = map[string]func(a, b library.Track) int{
	"title":    func(a, b library.Track) int { return cmpText(a.Title, b.Title) },
	"artist":   func(a, b library.Track) int { return cmpText(orUnknown(a.Artist), orUnknown(b.Artist)) },
	"album":    func(a, b library.Track) int { return 0 },
	"year":     func(a, b library.Track) int { return cmp.Compare(num(a.Year), num(b.Year)) },
	"added":    func(a, b library.Track) int { return cmp.Compare(a.DateAdded.UnixNano(), b.DateAdded.UnixNano()) },
	"duration": func(a, b library.Track) int { return cmp.Compare(a.Duration, b.Duration) },
	"size":     func(a, b library.Track) int { return cmp.Compare(a.FileSize, b.FileSize) },
	"format":   func(a, b library.Track) int { return cmpText(string(a.Format), string(b.Format)) },
	"path":     func(a, b library.Track) int { return cmpText(a.RelativePath, b.RelativePath) },
}

func sortTracks(tracks []library.Track, key string, reverse bool) {
	if key == "" {
		key = "album"
	}
	if key == "album" { // the whole album order flips
		sortBy(tracks, reverse, albumOrder, nil, albumOrder)
		return
	}
	var blank func(library.Track) bool
	if key == "year" {
		blank = func(t library.Track) bool { return t.Year == nil }
	}
	sortBy(tracks, reverse, trackKeys[key], blank, albumOrder)
}

// printTracks lists tracks, with their playlist positions when pos is set.
func printTracks(w *tabwriter.Writer, tracks []library.Track, pos map[meta.UUID]int) {
	head := "TITLE\tARTIST\tALBUM\t#\tLENGTH\tFORMAT\tADDED"
	if pos != nil {
		head = "POS\t" + head
	}
	fmt.Fprintln(w, head)
	for _, t := range tracks {
		if pos != nil {
			fmt.Fprintf(w, "%d\t", pos[t.ID])
		}
		n := ""
		if t.TrackNumber != nil {
			n = fmt.Sprint(*t.TrackNumber)
			if t.DiscNumber != nil && t.DiscTotal != nil && *t.DiscTotal > 1 {
				n = fmt.Sprintf("%d-%s", *t.DiscNumber, n)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.Title, orUnknown(t.Artist), str(t.Album), n,
			length(t.Duration), strings.ToUpper(string(t.Format)), t.DateAdded.Local().Format("2006-01-02"))
	}
}

func albumLength(a library.Album, byID map[meta.UUID]library.Track) (s float64) {
	for _, id := range a.TrackIDs {
		s += byID[id].Duration
	}
	return s
}

func albumAdded(a library.Album, byID map[meta.UUID]library.Track) (latest int64) {
	for _, id := range a.TrackIDs {
		latest = max(latest, byID[id].DateAdded.UnixNano())
	}
	return latest
}

func sortAlbums(albums []library.Album, byID map[meta.UUID]library.Track, key string, reverse bool) {
	cmp := map[string]func(a, b library.Album) int{
		"name":     func(a, b library.Album) int { return cmpText(a.Name, b.Name) },
		"artist":   func(a, b library.Album) int { return cmpText(orUnknown(a.Artist), orUnknown(b.Artist)) },
		"year":     func(a, b library.Album) int { return cmp.Compare(num(a.Year), num(b.Year)) },
		"tracks":   func(a, b library.Album) int { return cmp.Compare(len(a.TrackIDs), len(b.TrackIDs)) },
		"duration": func(a, b library.Album) int { return cmp.Compare(albumLength(a, byID), albumLength(b, byID)) },
		"added":    func(a, b library.Album) int { return cmp.Compare(albumAdded(a, byID), albumAdded(b, byID)) },
	}
	if key == "" {
		key = "name"
	}
	var blank func(library.Album) bool
	if key == "year" {
		blank = func(a library.Album) bool { return a.Year == nil }
	}
	sortBy(albums, reverse, cmp[key], blank, func(a, b library.Album) int { return cmpText(a.Name, b.Name) })
}

// printArtists lists the artists albums are filed under, as the apps'
// artist list does: album artist, else the lead artist, else Various Artists.
func printArtists(w *tabwriter.Writer, albums []library.Album, byID map[meta.UUID]library.Track, key string, reverse bool) {
	type artist struct {
		name           string
		albums, tracks int
		seconds        float64
	}
	index := map[string]int{}
	var list []artist
	for _, a := range albums {
		name := orUnknown(a.Artist)
		k := meta.ArtistKey(name)
		i, ok := index[k]
		if !ok {
			i = len(list)
			index[k] = i
			list = append(list, artist{name: name})
		}
		list[i].albums++
		list[i].tracks += len(a.TrackIDs)
		list[i].seconds += albumLength(a, byID)
	}
	cmp := map[string]func(a, b artist) int{
		"name":     func(a, b artist) int { return cmpText(a.name, b.name) },
		"albums":   func(a, b artist) int { return cmp.Compare(a.albums, b.albums) },
		"tracks":   func(a, b artist) int { return cmp.Compare(a.tracks, b.tracks) },
		"duration": func(a, b artist) int { return cmp.Compare(a.seconds, b.seconds) },
	}
	if key == "" {
		key = "name"
	}
	sortBy(list, reverse, cmp[key], nil, func(a, b artist) int { return cmpText(a.name, b.name) })
	fmt.Fprintln(w, "ARTIST\tALBUMS\tTRACKS\tLENGTH")
	for _, a := range list {
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", a.name, a.albums, a.tracks, length(a.seconds))
	}
}

// playlists lists them, or with view "playlist" the tracks of one named by
// name or ID. Entries whose file is gone show as missing.
func playlists(w *tabwriter.Writer, l *library.Library, o opts, view string, all map[meta.UUID]library.Track, kept []library.Track) error {
	raw, err := l.Playlists()
	if err != nil {
		return err
	}
	byPath := make(map[string]library.Track, len(all))
	for _, t := range all {
		byPath[t.RelativePath] = t
	}
	var list []meta.Playlist
	for _, b := range raw {
		var p meta.Playlist
		if json.Unmarshal(b, &p) == nil {
			list = append(list, p)
		}
	}
	if view == "playlists" {
		cmp := map[string]func(a, b meta.Playlist) int{
			"name":    func(a, b meta.Playlist) int { return cmpText(a.Name, b.Name) },
			"tracks":  func(a, b meta.Playlist) int { return cmp.Compare(len(a.Entries), len(b.Entries)) },
			"created": func(a, b meta.Playlist) int { return cmp.Compare(a.DateCreated.UnixNano(), b.DateCreated.UnixNano()) },
		}
		key := o.sort
		if key == "" {
			key = "name"
		}
		sortBy(list, o.reverse, cmp[key], nil, func(a, b meta.Playlist) int { return cmpText(a.Name, b.Name) })
		if len(list) == 0 {
			fmt.Printf("%s has no playlists.\n", o.args[2])
			return nil
		}
		fmt.Fprintln(w, "PLAYLIST\tTRACKS\tLENGTH\tCREATED\tID")
		for _, p := range list {
			var seconds float64
			for _, e := range p.Entries {
				if t, ok := resolve(e, all, byPath); ok {
					seconds += t.Duration
				}
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", p.Name, len(p.Entries), length(seconds), p.DateCreated.Local().Format("2006-01-02"), p.ID)
		}
		return nil
	}

	want := o.args[3]
	var found []meta.Playlist
	for _, p := range list {
		if p.ID.String() == strings.ToUpper(want) || meta.CaseInsensitiveEqual(p.Name, want) {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return fmt.Errorf("no playlist %q; see: flactastic-server library playlists %s", want, o.args[2])
	case 1:
	default:
		return fmt.Errorf("%d playlists are called %q; name one by its ID from: flactastic-server library playlists %s", len(found), want, o.args[2])
	}
	keep := trackMap(kept)
	var tracks []library.Track
	var missing []string
	pos := map[meta.UUID]int{}
	for i, e := range found[0].Entries {
		t, ok := resolve(e, all, byPath)
		if !ok {
			missing = append(missing, fmt.Sprintf("%d %s", i+1, e.RelativePath))
		} else if _, ok := keep[t.ID]; ok {
			if _, dup := pos[t.ID]; !dup {
				pos[t.ID] = i + 1
			}
			tracks = append(tracks, t)
		}
	}
	if o.sort != "" && o.sort != "position" {
		sortTracks(tracks, o.sort, o.reverse)
	} else if o.reverse {
		for i, j := 0, len(tracks)-1; i < j; i, j = i+1, j-1 {
			tracks[i], tracks[j] = tracks[j], tracks[i]
		}
	}
	printTracks(w, tracks, pos)
	w.Flush()
	for _, p := range missing {
		fmt.Fprintln(os.Stderr, "missing at "+p)
	}
	return nil
}

// resolve finds an entry's track by ID, else by path, as the apps do.
func resolve(e meta.PlaylistEntry, all map[meta.UUID]library.Track, byPath map[string]library.Track) (library.Track, bool) {
	if e.TrackID != nil {
		if t, ok := all[*e.TrackID]; ok {
			return t, true
		}
	}
	t, ok := byPath[e.RelativePath]
	return t, ok
}

// stats sums up the (filtered) library.
func stats(w *tabwriter.Writer, u store.User, l *library.Library, tracks []library.Track) error {
	var bytes int64
	var seconds float64
	unhashed := 0
	formats := map[string]int{}
	for _, t := range tracks {
		bytes += t.FileSize
		seconds += t.Duration
		formats[strings.ToUpper(string(t.Format))]++
		if t.ContentHash == nil {
			unhashed++
		}
	}
	albums := library.GroupAlbums(tracks)
	artists := map[string]bool{}
	for _, a := range albums {
		artists[meta.ArtistKey(orUnknown(a.Artist))] = true
	}
	raw, err := l.Playlists()
	if err != nil {
		return err
	}
	var names []string
	for f := range formats {
		names = append(names, f)
	}
	sort.Slice(names, func(i, j int) bool {
		return formats[names[i]] > formats[names[j]] || formats[names[i]] == formats[names[j]] && names[i] < names[j]
	})
	var mix []string
	for _, f := range names {
		mix = append(mix, fmt.Sprintf("%s %d", f, formats[f]))
	}
	for _, row := range [][2]string{
		{"User", u.Name},
		{"Music folder", u.Root},
		{"Tracks", fmt.Sprint(len(tracks))},
		{"Albums", fmt.Sprint(len(albums))},
		{"Artists", fmt.Sprint(len(artists))},
		{"Playlists", fmt.Sprint(len(raw))},
		{"Length", length(seconds)},
		{"Size", size(float64(bytes))},
		{"Formats", strings.Join(mix, ", ")},
		{"Not yet hashed", fmt.Sprint(unhashed)},
	} {
		fmt.Fprintf(w, "%s:\t%s\n", row[0], row[1])
	}
	return nil
}
