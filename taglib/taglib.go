// Package taglib reads and writes tags through TagLib 2.3.2, the Mac app's
// exact pin, following LibraryScanner.parse and MetadataWriter.write.
package taglib

/*
#cgo CXXFLAGS: -std=c++17 -O2 -I${SRCDIR}/../.deps/taglib/include
#cgo CFLAGS: -I${SRCDIR}/../.deps/taglib/include
#cgo LDFLAGS: -L${SRCDIR}/../.deps/taglib/lib -ltag_c -ltag -lz -lstdc++ -lm
#include <stdlib.h>
#include "open.h"
#include "taglib_helpers.h"
*/
import "C"

import (
	"errors"
	"strconv"
	"strings"
	"unsafe"

	"flactastic/server/meta"
)

func init() { C.taglib_set_string_management_enabled(0) }

var ErrOpen = errors.New("taglib: unreadable file")

type File struct {
	h *C.fts_handle
	f *C.TagLib_File
}

// Open takes ownership of fd. ext picks the parser, as TagLib's FileRef
// does from a path.
func Open(fd int, ext string, write bool) (*File, error) {
	types := map[string]C.int{"flac": C.FTS_FLAC, "mp3": C.FTS_MPEG, "aac": C.FTS_MPEG, "m4a": C.FTS_MP4,
		"wav": C.FTS_WAV, "wave": C.FTS_WAV, "aif": C.FTS_AIFF, "aiff": C.FTS_AIFF}
	t, ok := types[strings.ToLower(ext)]
	readOnly := C.int(1)
	if write {
		readOnly = 0
	}
	var h *C.fts_handle
	if ok {
		h = C.fts_open(C.int(fd), t, readOnly)
	}
	if h == nil {
		return nil, ErrOpen
	}
	file := &File{h: h, f: C.fts_file(h)}
	if C.taglib_file_is_valid(file.f) == 0 {
		file.Close()
		return nil, ErrOpen
	}
	return file, nil
}

func (f *File) Close() { C.fts_close(f.h) }

func (f *File) Save() error {
	if C.taglib_file_save(f.f) == 0 {
		return errors.New("taglib: save failed")
	}
	return nil
}

type Audio struct {
	DurationMs, SampleRate, Channels, Bitrate, Bits int
	Codec                                           string // "aac" or "alac" for MP4
	Frames                                          uint64 // exact for FLAC, WAV and AIFF, else from the duration
}

type Tags struct {
	Title                             string // "" when untagged; callers fall back to the file name
	Album, Artist, AlbumArtist, Genre *string
	ArtistIsList, AlbumArtistIsList   bool
	SecondaryGenres                   []string
	TagVersion, Year, TrackNumber     *int
	Compilation, MixCompilation       bool
	Label                             *string
	DiscNumber, DiscTotal             *int
	CueSheet                          string
	Audio
}

func (f *File) Read() Tags {
	var t Tags
	if tag := C.taglib_file_tag(f.f); tag != nil {
		t.Title = take(C.taglib_tag_title(tag))
		if s := take(C.taglib_tag_album(tag)); s != "" {
			t.Album = &s
		}
		f.readMultiValue(&t, tag)
		if y := int(C.taglib_tag_year(tag)); y > 0 {
			t.Year = &y
		}
		if n := int(C.taglib_tag_track(tag)); n > 0 {
			t.TrackNumber = &n
		}
	}
	t.Compilation = C.taglib_helper_get_compilation(unsafe.Pointer(f.f)) != 0
	t.MixCompilation = C.taglib_helper_get_mix_compilation(unsafe.Pointer(f.f)) != 0
	t.Label = f.property("LABEL")
	total := f.property("DISCTOTAL")
	if total == nil {
		total = f.property("TOTALDISCS")
	}
	t.DiscNumber, t.DiscTotal = meta.ParseDisc(f.property("DISCNUMBER"), total)
	if c := C.taglib_helper_get_cuesheet(unsafe.Pointer(f.f)); c != nil {
		t.CueSheet = C.GoString(c)
		C.free(unsafe.Pointer(c))
	}
	var a C.fts_audio_t
	C.fts_audio(f.h, &a)
	t.Audio = Audio{DurationMs: int(a.length_ms), SampleRate: int(a.sample_rate), Channels: int(a.channels),
		Bitrate: int(a.bitrate), Bits: int(a.bits), Codec: [...]string{"", "aac", "alac"}[a.codec], Frames: uint64(a.frames)}
	if t.Frames == 0 {
		t.Frames = uint64(t.DurationMs) * uint64(t.SampleRate) / 1000
	}
	return t
}

// readMultiValue is LibraryScanner.multiValueFields.
func (f *File) readMultiValue(t *Tags, tag *C.TagLib_Tag) {
	basic := func(p *C.char) []string {
		if s := take(p); s != "" {
			return []string{s}
		}
		return nil
	}
	present := map[string]bool{}
	for _, k := range f.keys() {
		present[k] = true
	}
	list := func(key string) []string {
		if !present[key] {
			return nil
		}
		return f.PropertyList(key)
	}
	native := func(key string, fallback func() []string) []string {
		if v := list(key); len(v) > 0 {
			return v
		}
		return fallback()
	}
	none := func() []string { return nil }
	artistIDs := meta.SplitIdentifiers(list("MUSICBRAINZ_ARTISTID"))
	albumArtistIDs := meta.SplitIdentifiers(list("MUSICBRAINZ_ALBUMARTISTID"))
	t.Artist, t.ArtistIsList = meta.StoredCredit(list("ARTISTS"),
		native("ARTIST", func() []string { return basic(C.taglib_tag_artist(tag)) }), len(artistIDs))
	t.AlbumArtist, t.AlbumArtistIsList = meta.StoredCredit(list("ALBUMARTISTS"), native("ALBUMARTIST", none), len(albumArtistIDs))
	t.Genre, t.SecondaryGenres = meta.StoredGenres(native("GENRE", func() []string { return basic(C.taglib_tag_genre(tag)) }))
	if v := list("FLACTASTIC_TAGVERSION"); len(v) > 0 {
		if n, err := strconv.Atoi(meta.TrimWhitespace(v[0])); err == nil {
			t.TagVersion = &n
		}
	}
}

func (f *File) keys() []string {
	return strings0(C.taglib_property_keys(f.f), -1)
}

// PropertyList is every value of a property, capped like the Mac's at
// MaxValues + 1 so a hostile file can't make the parser walk forever.
func (f *File) PropertyList(key string) []string {
	k := C.CString(key)
	defer C.free(unsafe.Pointer(k))
	return strings0(C.taglib_property_get(f.f, k), meta.MaxValues+1)
}

func (f *File) property(key string) *string {
	k := C.CString(key)
	defer C.free(unsafe.Pointer(k))
	p := C.taglib_helper_get_property(unsafe.Pointer(f.f), k)
	if p == nil {
		return nil
	}
	s := meta.Trim(C.GoString(p))
	C.free(unsafe.Pointer(p))
	if s == "" {
		return nil
	}
	return &s
}

func (f *File) Picture() []byte {
	var n C.uint
	p := C.taglib_helper_read_picture(unsafe.Pointer(f.f), &n)
	if p == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoBytes(unsafe.Pointer(p), C.int(n))
}

func take(p *C.char) string {
	if p == nil {
		return ""
	}
	defer C.taglib_free(unsafe.Pointer(p))
	return C.GoString(p)
}

func strings0(p **C.char, limit int) []string {
	if p == nil {
		return nil
	}
	defer C.taglib_property_free(p)
	var out []string
	for _, s := range unsafe.Slice(p, 1<<20) {
		if s == nil || len(out) == limit {
			break
		}
		out = append(out, C.GoString(s))
	}
	return out
}

func (f *File) Lyrics() string {
	p := C.taglib_helper_get_lyrics(unsafe.Pointer(f.f))
	if p == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoString(p)
}
