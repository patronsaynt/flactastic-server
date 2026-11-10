package library

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"path"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"flactastic/server/meta"
	"flactastic/server/taglib"
)

// Prefs follow a user between devices (protocol §14).
type Prefs struct {
	StreamQuality   *string                    `json:"streamQuality,omitempty"`
	ArtistSeparator *string                    `json:"artistSeparator,omitempty"`
	Clients         map[string]json.RawMessage `json:"clients,omitempty"`
}

var qualities = []string{"original", "auto", "high", "normal", "low"}

const maxClientPref = 64 << 10

func (p Prefs) Separator() string {
	if p.ArtistSeparator != nil {
		return *p.ArtistSeparator
	}
	return meta.DefaultDisplaySeparator
}

// Filled is p with every default written out.
func (p Prefs) Filled() Prefs {
	if p.StreamQuality == nil {
		q := "original"
		p.StreamQuality = &q
	}
	sep := p.Separator()
	p.ArtistSeparator = &sep
	if p.Clients == nil {
		p.Clients = map[string]json.RawMessage{}
	}
	return p
}

func (l *Library) PatchPrefs(in Prefs) (Prefs, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var p Prefs
	if err := l.Doc("prefs", &p); err != nil {
		return p, err
	}
	if in.StreamQuality != nil {
		if !slices.Contains(qualities, *in.StreamQuality) {
			return p, errors.New("unknown quality")
		}
		p.StreamQuality = in.StreamQuality
	}
	if in.ArtistSeparator != nil {
		if !slices.Contains(meta.DisplaySeparatorChoices, *in.ArtistSeparator) {
			return p, errors.New("unknown separator")
		}
		p.ArtistSeparator = in.ArtistSeparator
	}
	for k, v := range in.Clients {
		if p.Clients == nil {
			p.Clients = map[string]json.RawMessage{}
		}
		if string(v) == "null" {
			delete(p.Clients, k)
			continue
		}
		if len(k) > 64 || len(v) > maxClientPref || len(p.Clients) >= 64 {
			return p, errors.New("client preference too large")
		}
		p.Clients[k] = v
	}
	return p.Filled(), l.SetDoc("prefs", KindPrefs, p)
}

// ValidImage reports whether data is a JPEG or PNG of a sane size.
func ValidImage(data []byte) bool {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	return err == nil && (format == "jpeg" || format == "png") && cfg.Width > 0 && cfg.Height > 0 &&
		cfg.Width <= maxArtPixels && cfg.Height <= maxArtPixels
}

// Detail is a track with the tag text kept out of the track feed.
func (l *Library) Detail(id meta.UUID) (t Track, cue, lyrics string, err error) {
	if t, err = l.Track(id); err != nil {
		return t, "", "", ErrNotFound
	}
	f, err := l.OpenTrack(t)
	if err != nil {
		return t, "", "", nil
	}
	defer f.Close()
	dup, err := unix.Dup(int(f.Fd()))
	if err != nil {
		return t, "", "", nil
	}
	tf, err := taglib.Open(dup, strings.TrimPrefix(path.Ext(t.RelativePath), "."), false)
	if err != nil {
		return t, "", "", nil
	}
	defer tf.Close()
	return t, tf.Read().CueSheet, tf.Lyrics(), nil
}

// Manifests (protocol §13)

type Selection struct {
	AlbumIDs    []string    `json:"albumIDs"`
	PlaylistIDs []meta.UUID `json:"playlistIDs"`
	TrackIDs    []meta.UUID `json:"trackIDs"`
}

func (l *Library) Manifest(sel *Selection, f meta.SyncFilter) (meta.LibraryManifest, int, error) {
	m := meta.LibraryManifest{Tracks: []meta.TrackManifestEntry{}, Playlists: []meta.PlaylistManifestEntry{}}
	m.DeviceID, _ = meta.ParseUUID(l.User.Library)
	m.GeneratedAt = meta.Time{Time: time.Now()}
	tracks, err := l.allTracks()
	if err != nil {
		return m, 0, err
	}
	var playlists []meta.Playlist
	if raw, err := l.Playlists(); err == nil {
		for _, b := range raw {
			var p meta.Playlist
			if json.Unmarshal(b, &p) == nil {
				playlists = append(playlists, p)
			}
		}
	}
	want := map[meta.UUID]bool{}
	if sel != nil {
		for _, id := range sel.TrackIDs {
			want[id] = true
		}
		albums := map[string]bool{}
		for _, id := range sel.AlbumIDs {
			albums[id] = true
		}
		for _, a := range groupAlbums(tracks) {
			if albums[a.ID] {
				for _, id := range a.TrackIDs {
					want[id] = true
				}
			}
		}
		named := map[meta.UUID]bool{}
		for _, id := range sel.PlaylistIDs {
			named[id] = true
		}
		kept := playlists[:0]
		for _, p := range playlists {
			if named[p.ID] {
				kept = append(kept, p)
				for _, e := range p.Entries {
					if e.TrackID != nil {
						want[*e.TrackID] = true
					}
				}
			}
		}
		playlists = kept
	}
	pending := 0
	for _, t := range tracks {
		if sel != nil && !want[t.ID] {
			continue
		}
		var key *string
		if a := or(t.AlbumArtist, or(t.Artist, "")); meta.TrimWhitespace(a) != "" {
			k := meta.ArtistKey(a)
			key = &k
		}
		if !f.Allows(t.Format, key, t.FileSize) || t.FileSize > MaxFileBytes {
			continue
		}
		if t.ContentHash == nil {
			pending++
			continue
		}
		m.Tracks = append(m.Tracks, meta.TrackManifestEntry{TrackID: t.ID, RelativePath: t.RelativePath,
			FileSize: t.FileSize, ContentHash: *t.ContentHash, Format: t.Format, TagFingerprint: t.TagFingerprint,
			Title: t.Title, Artist: t.Artist, Album: t.Album, AlbumArtist: t.AlbumArtist})
	}
	for _, p := range playlists {
		if f.AllowsPlaylist(p.ID) {
			m.Playlists = append(m.Playlists, meta.PlaylistManifest(p))
		}
	}
	return m, pending, nil
}
