package library

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"flactastic/server/meta"
)

func (l *Library) allTracks() ([]Track, error) {
	var out []Track
	err := l.Tracks(func(data []byte) error {
		var t Track
		err := json.Unmarshal(data, &t)
		out = append(out, t)
		return err
	})
	return out, err
}

type Album struct {
	ID               string      `json:"id"`
	Name             string      `json:"name"`
	Artist           *string     `json:"artist,omitempty"`
	AlbumArtist      *string     `json:"albumArtist,omitempty"`
	Year             *int        `json:"year,omitempty"`
	Genre            *string     `json:"genre,omitempty"`
	SecondaryGenres  []string    `json:"secondaryGenres"`
	Label            *string     `json:"label,omitempty"`
	ArtworkID        *string     `json:"artworkID,omitempty"`
	IsCompilation    bool        `json:"isCompilation"`
	IsMixCompilation bool        `json:"isMixCompilation"`
	TrackIDs         []meta.UUID `json:"trackIDs"`
	tracks           []Track
}

func or(p *string, d string) string {
	if p == nil {
		return d
	}
	return *p
}

// Albums groups tracks as the Mac's LibraryStore.buildAlbums does.
func (l *Library) Albums() ([]Album, error) {
	tracks, err := l.allTracks()
	if err != nil {
		return nil, err
	}
	return groupAlbums(tracks), nil
}

func groupAlbums(tracks []Track) []Album {
	byName := map[string][]Track{}
	var names []string
	for _, t := range tracks {
		k := meta.Lower(or(t.Album, "Unknown Album"))
		if byName[k] == nil {
			names = append(names, k)
		}
		byName[k] = append(byName[k], t)
	}
	var out []Album
	for _, name := range names {
		group := byName[name]
		subgroups := [][]Track{group}
		for _, t := range group {
			if t.AlbumArtist != nil {
				by := map[string][]Track{}
				var keys []string
				for _, t := range group {
					k := or(t.AlbumArtist, "Unknown Artist")
					if by[k] == nil {
						keys = append(keys, k)
					}
					by[k] = append(by[k], t)
				}
				subgroups = subgroups[:0]
				for _, k := range keys {
					subgroups = append(subgroups, by[k])
				}
				break
			}
		}
		for _, g := range subgroups {
			out = append(out, makeAlbum(g))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return meta.StandardLess(out[i].Name, out[j].Name) })
	return out
}

func makeAlbum(g []Track) Album {
	sort.SliceStable(g, func(i, j int) bool {
		di, dj := intOr(g[i].DiscNumber, 1), intOr(g[j].DiscNumber, 1)
		if di != dj {
			return di < dj
		}
		return intOr(g[i].TrackNumber, 1<<62) < intOr(g[j].TrackNumber, 1<<62)
	})
	a := Album{Name: or(g[0].Album, "Unknown Album"), Year: g[0].Year, Genre: g[0].Genre,
		SecondaryGenres: g[0].SecondaryGenres, tracks: g}
	distinct := map[string]bool{}
	var lead string
	art := map[string]int{}
	for _, t := range g {
		if a.AlbumArtist == nil && t.AlbumArtist != nil {
			a.AlbumArtist = t.AlbumArtist
		}
		if a.Label == nil {
			a.Label = t.Label
		}
		if t.Artist != nil {
			lead = primaryCredit(*t.Artist)
			distinct[lead] = true
		}
		a.IsCompilation = a.IsCompilation || t.IsCompilation
		a.IsMixCompilation = a.IsMixCompilation || t.IsMixCompilation
		if t.ArtworkID != nil {
			art[*t.ArtworkID]++
			if a.ArtworkID == nil || art[*t.ArtworkID] > art[*a.ArtworkID] {
				a.ArtworkID = t.ArtworkID
			}
		}
		a.TrackIDs = append(a.TrackIDs, t.ID)
	}
	switch {
	case a.AlbumArtist != nil:
		a.Artist = a.AlbumArtist
	case len(distinct) > 1:
		v := "Various Artists"
		a.Artist = &v
	case len(distinct) == 1:
		a.Artist = &lead
	}
	key := "Unknown Artist"
	if a.Artist != nil {
		key = *a.Artist
	}
	a.ID = key + "|" + a.Name
	return a
}

// primaryCredit is ArtistResolver.primaryCredit.
func primaryCredit(raw string) string {
	if pieces, ok := meta.SplitSafe(raw); ok && len(pieces) > 0 {
		return pieces[0]
	}
	return meta.Trim(raw)
}

func intOr(p *int, d int) int {
	if p == nil {
		return d
	}
	return *p
}

// Search matches case- and diacritic-insensitively on titles and credits.
func (l *Library) Search(q string, limit int) ([]Track, error) {
	needle := meta.ArtistKey(q)
	var out []Track
	if needle == "" {
		return out, nil
	}
	err := l.Tracks(func(data []byte) error {
		if len(out) >= limit {
			return nil
		}
		var t Track
		if err := json.Unmarshal(data, &t); err != nil {
			return err
		}
		for _, f := range []string{t.Title, or(t.Artist, ""), or(t.AlbumArtist, ""), or(t.Album, "")} {
			if strings.Contains(meta.ArtistKey(f), needle) {
				out = append(out, t)
				break
			}
		}
		return nil
	})
	return out, err
}

// Playlists

func (l *Library) Playlists() ([]json.RawMessage, error) {
	rows, err := l.db.Query("SELECT data FROM playlists ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		rows.Scan(&b)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (l *Library) Playlist(id meta.UUID) (meta.Playlist, error) {
	var p meta.Playlist
	var b []byte
	err := l.db.QueryRow("SELECT data FROM playlists WHERE id = ?", id.String()).Scan(&b)
	if err == sql.ErrNoRows {
		return p, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	return p, err
}

func (l *Library) PutPlaylist(p meta.Playlist) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return l.write(func(tx *sql.Tx, _ int64) error {
		_, err := tx.Exec("INSERT OR REPLACE INTO playlists VALUES(?, ?)", p.ID.String(), data)
		return err
	}, KindPlaylists)
}

func (l *Library) DeletePlaylist(id meta.UUID) error {
	return l.write(func(tx *sql.Tx, _ int64) error {
		r, err := tx.Exec("DELETE FROM playlists WHERE id = ?", id.String())
		if n, _ := r.RowsAffected(); err == nil && n == 0 {
			err = ErrNotFound
		}
		return err
	}, KindPlaylists)
}

// History (protocol §15)

type PlayEvent struct {
	ID              meta.UUID `json:"id"`
	Date            meta.Time `json:"date"`
	TrackID         meta.UUID `json:"trackID"`
	Title           string    `json:"title"`
	Artist          *string   `json:"artist,omitempty"`
	AlbumID         *string   `json:"albumID,omitempty"`
	Album           *string   `json:"album,omitempty"`
	Genre           *string   `json:"genre,omitempty"`
	SecondaryGenres []string  `json:"secondaryGenres,omitempty"`
	SecondsListened float64   `json:"secondsListened"`
	Counted         bool      `json:"counted"`
}

type RecentContext struct {
	Kind     string    `json:"kind"`
	TargetID string    `json:"targetID"`
	Title    string    `json:"title"`
	Subtitle string    `json:"subtitle"`
	Date     meta.Time `json:"date"`
}

const maxContexts = 50

func (l *Library) AddHistory(events []PlayEvent, contexts []RecentContext) (accepted int, err error) {
	err = l.write(func(tx *sql.Tx, seq int64) error {
		for _, e := range events {
			data, _ := json.Marshal(e)
			r, err := tx.Exec("INSERT OR IGNORE INTO history VALUES(?, ?, ?, ?)", e.ID.String(), e.Date.UTC().Format(time.RFC3339Nano), seq, data)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			accepted += int(n)
		}
		for _, c := range contexts {
			data, _ := json.Marshal(c)
			if _, err := tx.Exec(`INSERT INTO contexts VALUES(?, ?, ?, ?, ?) ON CONFLICT(kind, target)
				DO UPDATE SET date = excluded.date, seq = excluded.seq, data = excluded.data WHERE excluded.date > contexts.date`,
				c.Kind, c.TargetID, c.Date.UTC().Format(time.RFC3339Nano), seq, data); err != nil {
				return err
			}
		}
		_, err := tx.Exec("DELETE FROM contexts WHERE rowid NOT IN (SELECT rowid FROM contexts ORDER BY date DESC LIMIT ?)", maxContexts)
		return err
	}, KindHistory)
	return
}

func (l *Library) HistorySince(since int64, limit int) (events, contexts []json.RawMessage, seq int64, more bool, err error) {
	current := l.Seq()
	rows, err := l.db.Query("SELECT seq, data FROM history WHERE seq > ? ORDER BY seq LIMIT ?", since, limit+1)
	if err != nil {
		return
	}
	seq = since
	for rows.Next() {
		if len(events) == limit {
			more = true
			break
		}
		var b []byte
		rows.Scan(&seq, &b)
		events = append(events, b)
	}
	rows.Close()
	if !more {
		seq = max(seq, current)
	}
	rows, err = l.db.Query("SELECT data FROM contexts ORDER BY date DESC")
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		rows.Scan(&b)
		contexts = append(contexts, b)
	}
	return events, contexts, seq, more, rows.Err()
}

func (l *Library) events() ([]PlayEvent, error) {
	rows, err := l.db.Query("SELECT data FROM history ORDER BY date")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlayEvent
	for rows.Next() {
		var b []byte
		var e PlayEvent
		rows.Scan(&b)
		if json.Unmarshal(b, &e) == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}
