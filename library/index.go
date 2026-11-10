package library

import (
	"database/sql"
	"encoding/json"

	"flactastic/server/meta"
)

var schema = []string{`
CREATE TABLE state(key TEXT PRIMARY KEY, value INTEGER NOT NULL);
INSERT INTO state VALUES('seq', 0);
CREATE TABLE tracks(
	id TEXT PRIMARY KEY, path TEXT UNIQUE, seq INTEGER NOT NULL,
	size INTEGER NOT NULL DEFAULT 0, mtime INTEGER NOT NULL DEFAULT 0, inode INTEGER NOT NULL DEFAULT 0,
	hash TEXT, art TEXT, data TEXT NOT NULL);
CREATE INDEX tracks_seq ON tracks(seq);
CREATE INDEX tracks_hash ON tracks(hash);
CREATE INDEX tracks_art ON tracks(art);
CREATE INDEX tracks_unhashed ON tracks(id) WHERE hash IS NULL AND path IS NOT NULL;
CREATE TABLE playlists(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE history(id TEXT PRIMARY KEY, date TEXT NOT NULL, seq INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX history_seq ON history(seq);
CREATE TABLE contexts(kind TEXT NOT NULL, target TEXT NOT NULL, date TEXT NOT NULL, seq INTEGER NOT NULL, data TEXT NOT NULL,
	PRIMARY KEY(kind, target));
CREATE TABLE docs(key TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE uploads(id TEXT PRIMARY KEY, ext TEXT NOT NULL, size INTEGER NOT NULL, hash TEXT NOT NULL, track TEXT,
	created INTEGER NOT NULL)`}

// Track is a track as clients see it (protocol §7).
type Track struct {
	ID                meta.UUID   `json:"id"`
	RelativePath      string      `json:"relativePath"`
	Format            meta.Format `json:"format"`
	FileSize          int64       `json:"fileSize"`
	ContentHash       *string     `json:"contentHash"`
	TagFingerprint    string      `json:"tagFingerprint"`
	Title             string      `json:"title"`
	Artist            *string     `json:"artist,omitempty"`
	ArtistIsList      bool        `json:"artistIsList"`
	AlbumArtist       *string     `json:"albumArtist,omitempty"`
	AlbumArtistIsList bool        `json:"albumArtistIsList"`
	Album             *string     `json:"album,omitempty"`
	TrackNumber       *int        `json:"trackNumber,omitempty"`
	DiscNumber        *int        `json:"discNumber,omitempty"`
	DiscTotal         *int        `json:"discTotal,omitempty"`
	Year              *int        `json:"year,omitempty"`
	Genre             *string     `json:"genre,omitempty"`
	SecondaryGenres   []string    `json:"secondaryGenres"`
	IsCompilation     bool        `json:"isCompilation"`
	IsMixCompilation  bool        `json:"isMixCompilation"`
	Label             *string     `json:"label,omitempty"`
	Duration          float64     `json:"duration"`
	SampleRate        int         `json:"sampleRate"`
	BitDepth          *int        `json:"bitDepth,omitempty"`
	Channels          int         `json:"channels"`
	Bitrate           int         `json:"bitrate"`
	Frames            uint64      `json:"frames"`
	ArtworkID         *string     `json:"artworkID,omitempty"`
	HasCueSheet       bool        `json:"hasCueSheet"`
	DateAdded         meta.Time   `json:"dateAdded"`
	Modified          meta.Time   `json:"modified"`
}

// Kinds of change reported by the change signal (protocol §11).
const (
	KindTracks    = "tracks"
	KindPlaylists = "playlists"
	KindPrefs     = "prefs"
	KindOrganizer = "organizer"
	KindHistory   = "history"
	KindLibrary   = "library"
)

var Kinds = []string{KindTracks, KindPlaylists, KindPrefs, KindOrganizer, KindHistory, KindLibrary}

// bump advances the change sequence for kinds inside tx and returns it.
func bump(tx *sql.Tx, kinds ...string) (seq int64, err error) {
	if err = tx.QueryRow("UPDATE state SET value = value + 1 WHERE key = 'seq' RETURNING value").Scan(&seq); err != nil {
		return
	}
	for _, k := range kinds {
		if _, err = tx.Exec("INSERT OR REPLACE INTO state VALUES(?, ?)", "seq."+k, seq); err != nil {
			return
		}
	}
	return
}

// Seq is the current change sequence.
func (l *Library) Seq() (seq int64) {
	l.db.QueryRow("SELECT value FROM state WHERE key = 'seq'").Scan(&seq)
	return
}

// Changed lists the kinds changed after since.
func (l *Library) Changed(since int64) (seq int64, kinds []string) {
	rows, err := l.db.Query("SELECT key, value FROM state")
	if err != nil {
		return 0, nil
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v int64
		rows.Scan(&k, &v)
		if k == "seq" {
			seq = v
		} else if v > since {
			kinds = append(kinds, k[len("seq."):])
		}
	}
	return
}

// write runs fn in a transaction after advancing the change sequence for
// kinds (seq is 0 with no kinds), then wakes long-polls.
func (l *Library) write(fn func(tx *sql.Tx, seq int64) error, kinds ...string) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int64
	if len(kinds) > 0 {
		if seq, err = bump(tx, kinds...); err != nil {
			return err
		}
	}
	if err = fn(tx, seq); err != nil {
		return err
	}
	if err = tx.Commit(); err == nil && len(kinds) > 0 {
		l.notify()
	}
	return err
}

func (l *Library) Track(id meta.UUID) (Track, error) {
	var data string
	err := l.db.QueryRow("SELECT data FROM tracks WHERE id = ? AND path IS NOT NULL", id.String()).Scan(&data)
	var t Track
	if err == nil {
		err = json.Unmarshal([]byte(data), &t)
	}
	return t, err
}

// Tracks streams the raw JSON of every live track to fn.
func (l *Library) Tracks(fn func(data []byte) error) error {
	rows, err := l.db.Query("SELECT data FROM tracks WHERE path IS NOT NULL")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
	}
	return rows.Err()
}

// TracksSince is one page of the track feed (protocol §7).
func (l *Library) TracksSince(since int64, limit int) (tracks []json.RawMessage, deleted []string, seq int64, more bool, err error) {
	// Read the sequence first: everything at or below it is already committed.
	current := l.Seq()
	rows, err := l.db.Query("SELECT id, seq, path IS NULL, data FROM tracks WHERE seq > ? ORDER BY seq LIMIT ?", since, limit+1)
	if err != nil {
		return
	}
	defer rows.Close()
	seq = since
	n := 0
	for rows.Next() {
		if n == limit {
			more = true
			break
		}
		var id string
		var gone bool
		var data []byte
		if err = rows.Scan(&id, &seq, &gone, &data); err != nil {
			return
		}
		if gone {
			deleted = append(deleted, id)
		} else {
			tracks = append(tracks, data)
		}
		n++
	}
	if !more {
		seq = max(seq, current)
	}
	return tracks, deleted, seq, more, rows.Err()
}

// Doc reads a stored JSON document (prefs, organizer) into v.
func (l *Library) Doc(key string, v any) error {
	var data []byte
	if err := l.db.QueryRow("SELECT data FROM docs WHERE key = ?", key).Scan(&data); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, v)
}

func (l *Library) SetDoc(key, kind string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return l.write(func(tx *sql.Tx, _ int64) error {
		_, err := tx.Exec("INSERT OR REPLACE INTO docs VALUES(?, ?)", key, data)
		return err
	}, kind)
}
