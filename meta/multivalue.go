package meta

import (
	"slices"
	"strings"
)

const (
	MaxValueLength     = 1024
	MaxValues          = 50
	MaxSecondaryGenres = 3
)

type Field string

const (
	FieldArtist      Field = "artist"
	FieldAlbumArtist Field = "albumArtist"
	FieldGenre       Field = "genre"
)

// Decision is a saved review answer: Keep, or split into Parts.
type Decision struct {
	Keep  bool
	Parts []string
}

// Outcome is the field's values, or (Review != "") a raw string kept whole
// pending review.
type Outcome struct {
	Values []string
	Review string
}

var safeSeparators = []string{"\x00", ";", " / "}

var ambiguousSeparators = []string{
	" featuring ", " feat. ", " feat ", " ft. ", " ft ",
	" vs. ", " vs ",
	" with ",
	" & ", " x ", " X ",
	", ",
	"&", ",", "/",
}

var (
	DisplaySeparatorChoices = []string{", ", "; ", " ; ", " / ", " & "}
	DefaultDisplaySeparator = ", "
	keepableSeparators      = []string{" ; ", "; ", ";", " / ", "/", ", ", ",", " & ", "&", " + ", " x ", " \\ ", "\\\\", "\\"}
)

func trimmedPieces(pieces []string) []string {
	out := []string{}
	for _, p := range pieces {
		if p = Trim(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SplitSafe splits on the highest-priority safe separator; false when none.
func SplitSafe(raw string) ([]string, bool) {
	for _, d := range safeSeparators {
		if strings.Contains(raw, d) {
			return trimmedPieces(strings.Split(raw, d)), true
		}
	}
	if strings.Contains(raw, "\\") {
		return trimmedPieces(splitGraphemes(raw, func(g string) bool { return g == "\\" }, true)), true
	}
	return nil, false
}

func HasAmbiguousSeparator(raw string) bool {
	padded := " " + raw + " "
	for _, t := range ambiguousSeparators {
		if i, _ := foldIndex(padded, t); i >= 0 {
			return true
		}
	}
	return false
}

func caseInsensitiveSplit(s, sep string) []string {
	var out []string
	for {
		i, j := foldIndex(s, sep)
		if i < 0 {
			return append(out, s)
		}
		out = append(out, s[:i])
		s = s[j:]
	}
}

func SplitAmbiguous(raw string) []string {
	fragments := []string{raw}
	for _, t := range ambiguousSeparators {
		var next []string
		for _, f := range fragments {
			next = append(next, caseInsensitiveSplit(f, t)...)
		}
		fragments = next
	}
	return trimmedPieces(fragments)
}

// SplitIdentifiers splits MusicBrainz ID lists on any separator or whitespace.
func SplitIdentifiers(values []string) []string {
	out := []string{}
	for _, v := range values {
		out = append(out, strings.FieldsFunc(v, func(r rune) bool {
			return strings.ContainsRune("\x00;,/\\", r) || isSpace(r) || isNewline(r)
		})...)
	}
	return out
}

// Normalize trims, drops empties, dedupes case-insensitively (first casing
// wins) and applies the length and count caps.
func Normalize(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		v = Trim(v)
		if v == "" {
			continue
		}
		v = capGraphemes(v, MaxValueLength)
		k := lowerKey(v)
		if seen[k] {
			continue
		}
		seen[k] = true
		if len(out) == MaxValues {
			break
		}
		out = append(out, v)
	}
	return out
}

// Resolve is MultiValueParser.resolve. isKnown may be nil.
func Resolve(field Field, plural, native []string, mbidCount int, isKnown func(string) bool, decision *Decision) Outcome {
	if p := Normalize(plural); len(p) > 0 {
		return Outcome{Values: p}
	}
	n := Normalize(native)
	if len(n) != 1 {
		return Outcome{Values: n}
	}
	raw := n[0]
	if safe, ok := SplitSafe(raw); ok {
		return Outcome{Values: Normalize(safe)}
	}
	if field == FieldGenre || !HasAmbiguousSeparator(raw) {
		return Outcome{Values: []string{raw}}
	}
	if decision != nil {
		if decision.Keep {
			return Outcome{Values: []string{raw}}
		}
		return Outcome{Values: Normalize(decision.Parts)}
	}
	parts := Normalize(SplitAmbiguous(raw))
	if len(parts) <= 1 {
		return Outcome{Values: []string{raw}}
	}
	known := isKnown != nil
	for _, p := range parts {
		known = known && isKnown(p)
	}
	if known || (mbidCount > 1 && mbidCount == len(parts)) {
		return Outcome{Values: parts}
	}
	return Outcome{Review: raw}
}

func capped(v string) string { return capGraphemes(v, MaxValueLength) }

func nonBlank(values []string) []string {
	var out []string
	for _, v := range values {
		if Trim(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

func lowerKeys(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = lowerKey(v)
	}
	return out
}

// StoredCredit is how an artist/album-artist field is held: the display
// string, and whether it came from a real list.
func StoredCredit(plural, native []string, mbidCount int) (*string, bool) {
	list := Normalize(plural)
	singles := nonBlank(native)
	if len(list) > 0 {
		if len(singles) == 1 {
			single := capped(singles[0])
			parsed, ok := SplitSafe(single)
			if !ok {
				parsed = []string{single}
			}
			if slices.Equal(lowerKeys(Normalize(parsed)), lowerKeys(list)) {
				return &single, true
			}
		}
		v := JoinList(list)
		return &v, true
	}
	if nl := Normalize(native); len(nl) > 1 {
		v := JoinList(nl)
		return &v, true
	}
	if len(singles) == 0 {
		return nil, false
	}
	raw := capped(singles[0])
	if _, safe := SplitSafe(raw); mbidCount > 1 && !safe && HasAmbiguousSeparator(raw) {
		if parts := Normalize(SplitAmbiguous(raw)); len(parts) == mbidCount {
			v := JoinList(parts)
			return &v, true
		}
	}
	return &raw, false
}

// StoredGenres is the genre field as held: primary plus up to three secondaries.
func StoredGenres(native []string) (*string, []string) {
	if list := Normalize(native); len(list) > 1 {
		j := JoinList(list)
		return GenreSplit(&j)
	}
	if s := nonBlank(native); len(s) > 0 {
		c := capped(s[0])
		return GenreSplit(&c)
	}
	return GenreSplit(nil)
}

func JoinList(values []string) string { return strings.Join(values, " ; ") }

// ValuesOfStored is the values a stored credit or genre string stands for.
func ValuesOfStored(raw string) []string {
	if Trim(raw) == "" {
		return []string{}
	}
	if v, ok := SplitSafe(raw); ok {
		return Normalize(v)
	}
	return Normalize([]string{raw})
}

// Separator is MultiValueParser.separator(in:joining:): the recognisable
// list separator between the first two values in display.
func Separator(display string, values []string) (string, bool) {
	if len(values) < 2 {
		return "", false
	}
	s := Trim(display)
	n, ok := foldPrefix(s, values[0])
	if !ok {
		return "", false
	}
	rest := s[n:]
	i, _ := foldIndex(rest, values[1])
	if i < 0 {
		return "", false
	}
	if between := rest[:i]; slices.Contains(keepableSeparators, between) {
		return between, true
	}
	return "", false
}

// GenreSplit is GenreResolver.split.
func GenreSplit(raw *string) (*string, []string) {
	if raw == nil {
		return nil, []string{}
	}
	trimmed := Trim(*raw)
	if trimmed == "" {
		return nil, []string{}
	}
	pieces, ok := SplitSafe(trimmed)
	if !ok {
		return &trimmed, []string{}
	}
	if len(pieces) == 0 {
		return nil, []string{}
	}
	first := pieces[0]
	seen := map[string]bool{lowerKey(first): true}
	secondary := []string{}
	for _, p := range pieces[1:] {
		if len(secondary) >= MaxSecondaryGenres {
			break
		}
		if k := lowerKey(p); !seen[k] {
			seen[k] = true
			secondary = append(secondary, p)
		}
	}
	return &first, secondary
}

// GenreJoin is GenreResolver.join; nil when nothing survives.
func GenreJoin(primary *string, secondary []string) *string {
	seen := map[string]bool{}
	var all []string
	if primary != nil {
		if p := Trim(*primary); p != "" {
			seen[lowerKey(p)] = true
			all = append(all, p)
		}
	}
	n := 0
	for _, raw := range secondary {
		if n >= MaxSecondaryGenres {
			break
		}
		t := Trim(raw)
		if t == "" || seen[lowerKey(t)] {
			continue
		}
		seen[lowerKey(t)] = true
		all = append(all, t)
		n++
	}
	if len(all) == 0 {
		return nil
	}
	j := JoinList(all)
	return &j
}
