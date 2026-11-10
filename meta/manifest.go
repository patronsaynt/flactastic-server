package meta

import (
	"encoding/json"
	"slices"
	"strings"
)

type TrackManifestEntry struct {
	TrackID        UUID    `json:"trackID"`
	RelativePath   string  `json:"relativePath"`
	FileSize       int64   `json:"fileSize"`
	ContentHash    string  `json:"contentHash"`
	Format         Format  `json:"format"`
	TagFingerprint string  `json:"tagFingerprint"`
	Title          string  `json:"title"`
	Artist         *string `json:"artist,omitempty"`
	Album          *string `json:"album,omitempty"`
	AlbumArtist    *string `json:"albumArtist,omitempty"`
}

type PlaylistManifestEntry struct {
	ID          UUID   `json:"id"`
	Name        string `json:"name"`
	DateCreated Time   `json:"dateCreated"`
	EntryCount  int    `json:"entryCount"`
	ContentHash string `json:"contentHash"`
}

type LibraryManifest struct {
	DeviceID    UUID                    `json:"deviceID"`
	GeneratedAt Time                    `json:"generatedAt"`
	Tracks      []TrackManifestEntry    `json:"tracks"`
	Playlists   []PlaylistManifestEntry `json:"playlists"`
}

// SyncFilter: every field defaults when absent. A nil PlaylistIDAllowlist
// allows all playlists; an empty one allows none.
type SyncFilter struct {
	ExcludedFormats       []Format `json:"excludedFormats"`
	ExcludedArtistKeys    []string `json:"excludedArtistKeys"`
	MaxFileSizeBytes      *int64   `json:"maxFileSizeBytes,omitempty"`
	IncludePlaylists      bool     `json:"includePlaylists"`
	PlaylistIDAllowlist   []UUID   `json:"playlistIDAllowlist,omitzero"`
	MaxTotalTransferBytes *int64   `json:"maxTotalTransferBytes,omitempty"`
}

var Unrestricted = SyncFilter{IncludePlaylists: true}

func (f *SyncFilter) UnmarshalJSON(b []byte) error {
	type plain SyncFilter
	v := plain{IncludePlaylists: true}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = SyncFilter(v)
	return nil
}

func (f SyncFilter) MarshalJSON() ([]byte, error) {
	type plain SyncFilter
	v := plain(f)
	if v.ExcludedFormats == nil {
		v.ExcludedFormats = []Format{}
	}
	if v.ExcludedArtistKeys == nil {
		v.ExcludedArtistKeys = []string{}
	}
	return json.Marshal(v)
}

// Allows reports whether a track survives the filter; artistKey may be nil.
func (f SyncFilter) Allows(format Format, artistKey *string, fileSize int64) bool {
	if slices.Contains(f.ExcludedFormats, format) {
		return false
	}
	if artistKey != nil && slices.ContainsFunc(f.ExcludedArtistKeys, func(k string) bool { return NFC(k) == NFC(*artistKey) }) {
		return false
	}
	return f.MaxFileSizeBytes == nil || fileSize <= *f.MaxFileSizeBytes
}

func (f SyncFilter) AllowsPlaylist(id UUID) bool {
	return f.IncludePlaylists && (f.PlaylistIDAllowlist == nil || slices.Contains(f.PlaylistIDAllowlist, id))
}

type PlaylistEntry struct {
	ID           UUID   `json:"id"`
	TrackID      *UUID  `json:"trackID,omitempty"`
	RelativePath string `json:"relativePath"`
}

type Playlist struct {
	ID            UUID            `json:"id"`
	Name          string          `json:"name"`
	Entries       []PlaylistEntry `json:"entries"`
	DateCreated   Time            `json:"dateCreated"`
	CustomArtwork []byte          `json:"customArtwork,omitempty"`
	Description   *string         `json:"description,omitempty"`
}

// UnmarshalJSON falls back to the legacy trackPaths array, and to no
// entries when neither decodes.
func (p *Playlist) UnmarshalJSON(b []byte) error {
	type plain Playlist
	var v struct {
		plain
		Entries    json.RawMessage `json:"entries"`
		TrackPaths json.RawMessage `json:"trackPaths"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*p = Playlist(v.plain)
	p.Entries = []PlaylistEntry{}
	var paths []string
	if v.Entries != nil && json.Unmarshal(v.Entries, &p.Entries) == nil && p.Entries != nil {
		return nil
	}
	p.Entries = []PlaylistEntry{}
	if v.TrackPaths != nil && json.Unmarshal(v.TrackPaths, &paths) == nil {
		for _, path := range paths {
			p.Entries = append(p.Entries, PlaylistEntry{ID: NewUUID(), RelativePath: path})
		}
	}
	return nil
}

// PlaylistContentHash is ManifestBuilder.contentHash(for:): name and the
// ordered (trackID, relativePath) pairs.
func PlaylistContentHash(p Playlist) string {
	lines := make([]string, len(p.Entries))
	for i, e := range p.Entries {
		id := "-"
		if e.TrackID != nil {
			id = e.TrackID.String()
		}
		lines[i] = id + "|" + e.RelativePath
	}
	return sha256Hex(p.Name + "\n" + strings.Join(lines, "\n"))
}

func PlaylistManifest(p Playlist) PlaylistManifestEntry {
	return PlaylistManifestEntry{ID: p.ID, Name: p.Name, DateCreated: p.DateCreated,
		EntryCount: len(p.Entries), ContentHash: PlaylistContentHash(p)}
}
