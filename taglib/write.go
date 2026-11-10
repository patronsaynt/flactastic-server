package taglib

/*
#include <stdlib.h>
#include "open.h"
#include "taglib_helpers.h"
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	"flactastic/server/meta"
)

// Opt is a field of a partial edit: absent leaves the tag alone, null clears
// it, a value sets it.
type Opt[T any] struct {
	Set bool
	V   *T
}

func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.V = nil
		return nil
	}
	o.V = new(T)
	return json.Unmarshal(b, o.V)
}

// Edit carries the Mac metadata editor's field set.
type Edit struct {
	Title, Album, Artist, AlbumArtist, Genre, Label, Lyrics, CueSheet Opt[string]
	SecondaryGenres                                                   Opt[[]string]
	Year, TrackNumber, DiscNumber, DiscTotal                          Opt[int]
	Compilation                                                       Opt[bool] `json:"isCompilation"`
	MixCompilation                                                    Opt[bool] `json:"isMixCompilation"`
	Artwork                                                           Opt[[]byte]
}

const currentTagVersion = 2

// Apply writes e the way MetadataWriter.write does, with separator as the
// user's display separator for composed ARTIST strings.
func (f *File) Apply(e Edit, cur Tags, flac bool, separator string) {
	if tag := C.taglib_file_tag(f.f); tag != nil {
		if e.Title.Set {
			withC(deref(e.Title.V), func(s *C.char) { C.taglib_tag_set_title(tag, s) })
		}
		if e.Album.Set {
			withC(deref(e.Album.V), func(s *C.char) { C.taglib_tag_set_album(tag, s) })
		}
		if e.Year.Set {
			C.taglib_tag_set_year(tag, C.uint(max(0, deref(e.Year.V))))
		}
		if e.TrackNumber.Set {
			C.taglib_tag_set_track(tag, C.uint(max(0, deref(e.TrackNumber.V))))
		}
	}
	multi := false
	if e.Artist.Set {
		f.writeCredit("ARTIST", "ARTISTS", e.Artist.V, separator)
		multi = true
	}
	if e.AlbumArtist.Set {
		f.writeCredit("ALBUMARTIST", "ALBUMARTISTS", e.AlbumArtist.V, separator)
		multi = true
	}
	if e.Genre.Set || e.SecondaryGenres.Set {
		primary, secondary := cur.Genre, cur.SecondaryGenres
		if e.Genre.Set {
			primary = e.Genre.V
		}
		if e.SecondaryGenres.Set {
			secondary = deref(e.SecondaryGenres.V)
		}
		f.writeGenres(primary, secondary)
		multi = true
	}
	if multi {
		f.setList("FLACTASTIC_TAGVERSION", []string{strconv.Itoa(currentTagVersion)})
	}
	file := unsafe.Pointer(f.f)
	if e.Compilation.Set {
		on := deref(e.Compilation.V)
		C.taglib_helper_set_compilation(file, cbool(on))
		if on {
			C.taglib_helper_set_mix_compilation(file, 0)
		}
	}
	if e.MixCompilation.Set {
		on := deref(e.MixCompilation.V)
		C.taglib_helper_set_mix_compilation(file, cbool(on))
		if on {
			withC("", func(s *C.char) { C.taglib_helper_set_lyrics(file, s) })
			C.taglib_helper_set_compilation(file, 0)
		}
	}
	if e.Label.Set {
		f.setProperty("LABEL", e.Label.V)
	}
	if e.Lyrics.Set {
		withC(deref(e.Lyrics.V), func(s *C.char) { C.taglib_helper_set_lyrics(file, s) })
	}
	if e.CueSheet.Set {
		withC(deref(e.CueSheet.V), func(s *C.char) { C.taglib_helper_set_cuesheet(file, s) })
	}
	if e.DiscNumber.Set || e.DiscTotal.Set {
		number, total := cur.DiscNumber, cur.DiscTotal
		if e.DiscNumber.Set {
			number = e.DiscNumber.V
		}
		if e.DiscTotal.Set {
			total = e.DiscTotal.V
		}
		var n *string
		if number != nil {
			s := strconv.Itoa(*number)
			n = &s
		}
		switch {
		case flac:
			var t *string
			if n != nil && total != nil {
				s := strconv.Itoa(*total)
				t = &s
			}
			f.setProperty("DISCNUMBER", n)
			f.setProperty("DISCTOTAL", t)
			f.setProperty("TOTALDISCS", nil)
		case n != nil && total != nil:
			s := *n + "/" + strconv.Itoa(*total)
			f.setProperty("DISCNUMBER", &s)
		default:
			f.setProperty("DISCNUMBER", n)
		}
	}
	if e.Artwork.Set {
		C.taglib_helper_remove_pictures(file)
		if data := deref(e.Artwork.V); len(data) > 0 {
			mime := "image/jpeg"
			if bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G'}) {
				mime = "image/png"
			}
			m := C.CString(mime)
			C.taglib_helper_set_picture(file, (*C.char)(unsafe.Pointer(&data[0])), C.uint(len(data)), m)
			C.free(unsafe.Pointer(m))
		}
	}
}

func (f *File) writeCredit(singular, plural string, value *string, preferred string) {
	desired := meta.ValuesOfStored(deref(value))
	currentSingular := f.PropertyList(singular)
	currentPlural := f.PropertyList(plural)
	if len(currentSingular) > 1 && slices.Equal(meta.Normalize(currentSingular), desired) {
		return
	}
	if len(desired) == 0 {
		f.setList(singular, nil)
		f.setList(plural, nil)
		return
	}
	stored, _ := meta.StoredCredit(currentPlural, currentSingular, 0)
	currentValues := meta.ValuesOfStored(deref(stored))
	var display *string
	if !slices.Equal(currentValues, desired) || len(currentSingular) != 1 {
		var d string
		if len(desired) == 1 {
			d = meta.Trim(deref(value))
		} else {
			d = strings.Join(desired, displaySeparator(currentSingular, currentValues, desired, preferred))
		}
		display = &d
		f.setList(singular, []string{d})
	}
	if len(desired) > 1 {
		if !slices.Equal(meta.Normalize(currentPlural), desired) {
			f.setList(plural, desired)
			if !slices.Equal(f.PropertyList(plural), desired) {
				f.setList(plural, nil)
				if display != nil && !slices.Equal(meta.ValuesOfStored(*display), desired) {
					f.setList(singular, []string{meta.JoinList(desired)})
				}
			}
		}
	} else if len(currentPlural) > 0 {
		f.setList(plural, nil)
	}
}

func displaySeparator(existing, currentValues, desired []string, preferred string) string {
	if len(existing) != 1 {
		return preferred
	}
	raw := existing[0]
	parts := currentValues
	if len(currentValues) <= 1 {
		parts = meta.Normalize(meta.SplitAmbiguous(raw))
	}
	isList := len(currentValues) > 1
	if !isList && len(parts) > 1 {
		isList = true
		for _, p := range parts {
			found := false
			for _, d := range desired {
				found = found || meta.CaseInsensitiveEqual(d, p)
			}
			isList = isList && found
		}
	}
	if !isList {
		return preferred
	}
	if kept, ok := meta.Separator(raw, parts); ok {
		return kept
	}
	return preferred
}

func (f *File) writeGenres(primary *string, secondary []string) {
	joined := meta.GenreJoin(primary, secondary)
	desired := meta.ValuesOfStored(deref(joined))
	if slices.Equal(f.PropertyList("GENRE"), desired) {
		return
	}
	f.setList("GENRE", desired)
	if len(desired) > 1 && !slices.Equal(f.PropertyList("GENRE"), desired) {
		var one []string
		if joined != nil {
			one = []string{*joined}
		}
		f.setList("GENRE", one)
	}
}

func (f *File) setList(key string, values []string) {
	k := C.CString(key)
	defer C.free(unsafe.Pointer(k))
	if len(values) == 0 {
		C.taglib_property_set(f.f, k, nil)
		return
	}
	withC(values[0], func(s *C.char) { C.taglib_property_set(f.f, k, s) })
	for _, v := range values[1:] {
		withC(v, func(s *C.char) { C.taglib_property_set_append(f.f, k, s) })
	}
}

func (f *File) setProperty(key string, value *string) {
	k := C.CString(key)
	defer C.free(unsafe.Pointer(k))
	withC(deref(value), func(s *C.char) { C.taglib_helper_set_property(unsafe.Pointer(f.f), k, s) })
}

func withC(s string, fn func(*C.char)) {
	c := C.CString(s)
	defer C.free(unsafe.Pointer(c))
	fn(c)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}
