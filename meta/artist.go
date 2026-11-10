package meta

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// ArtistKey is ArtistResolver.key(for:): case- and diacritic-folded (full
// case folding, then nonspacing marks dropped), whitespace collapsed, trimmed.
func ArtistKey(raw string) string {
	f := norm.NFD.String(cases.Fold().String(raw))
	f = NFC(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, f))
	words := splitGraphemes(f, func(g string) bool {
		r := []rune(g)[0]
		return unicode.IsSpace(r)
	}, true)
	return Trim(strings.Join(words, " "))
}

// Credit is the artist fields of one track as ArtistResolver reads them.
type Credit struct {
	Artist, AlbumArtist             *string
	ArtistIsList, AlbumArtistIsList bool
}

// ArtistResolver splits credit strings against the names known to a library.
// ArtistResolver is not safe for concurrent use.
type ArtistResolver struct {
	standalone map[string]bool
	exact      map[string]bool
	decisions  map[string]Decision
	splits     map[string][]string // Split is pure, and a library repeats its artists
}

// NewArtistResolver builds a resolver; decisions (keyed by raw credit) may be nil.
func NewArtistResolver(credits []Credit, decisions map[string]Decision) *ArtistResolver {
	r := &ArtistResolver{standalone: map[string]bool{}, exact: map[string]bool{}, decisions: map[string]Decision{},
		splits: map[string][]string{}}
	for k, v := range decisions {
		r.decisions[NFC(k)] = v
	}
	for _, c := range credits {
		for _, f := range []struct {
			raw    *string
			isList bool
		}{{c.AlbumArtist, c.AlbumArtistIsList}, {c.Artist, c.ArtistIsList}} {
			if f.raw == nil || *f.raw == "" {
				continue
			}
			trimmed := Trim(*f.raw)
			if f.isList {
				r.exact[NFC(trimmed)] = true
			}
			d, hasD := r.decisions[NFC(trimmed)]
			if pieces, ok := SplitSafe(trimmed); ok {
				r.addStandalone(pieces...)
			} else if hasD && !d.Keep {
				r.addStandalone(d.Parts...)
			} else if f.isList || (hasD && d.Keep) || !HasAmbiguousSeparator(trimmed) {
				r.addStandalone(trimmed)
			}
		}
	}
	return r
}

func (r *ArtistResolver) addStandalone(names ...string) {
	for _, n := range names {
		r.standalone[NFC(ArtistKey(n))] = true
	}
}

func (r *ArtistResolver) isKnown(s string) bool { return r.standalone[NFC(ArtistKey(s))] }

// Split is ArtistResolver.split: a credit as one or more artist names.
func (r *ArtistResolver) Split(raw string) []string {
	if v, ok := r.splits[raw]; ok {
		return v
	}
	v := r.split(raw)
	r.splits[raw] = v
	return v
}

func (r *ArtistResolver) split(raw string) []string {
	trimmed := Trim(raw)
	if trimmed == "" {
		return []string{}
	}
	if explicit, ok := SplitSafe(trimmed); ok {
		return explicit
	}
	if r.exact[NFC(trimmed)] || r.isKnown(trimmed) {
		return []string{trimmed}
	}
	var d *Decision
	if v, ok := r.decisions[NFC(trimmed)]; ok {
		d = &v
	}
	if o := Resolve(FieldArtist, nil, []string{trimmed}, 0, r.isKnown, d); o.Review == "" && len(o.Values) > 0 {
		return o.Values
	}
	return []string{trimmed}
}
