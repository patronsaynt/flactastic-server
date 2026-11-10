package meta

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func ptr[T any](v T) *T { return &v }

func TestPathRejections(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                   "Path is empty.",
		"a\x00b":                             "Path contains a null byte.",
		"/etc/passwd":                        "Path is absolute.",
		`\\srv\x`:                            "Path is absolute.",
		"C:/x.flac":                          "Path is absolute.",
		"~/x.flac":                           "Path references a home directory.",
		"a/~b/x.flac":                        "Path references a home directory.",
		"a/../x.flac":                        "Path contains a parent-directory reference.",
		"a/.../x.flac":                       "Path contains a parent-directory reference.",
		"a/./x.flac":                         "Path contains a current-directory reference.",
		"a//x.flac":                          "Path contains an empty component.",
		"/":                                  "Path is absolute.",
		" ~/x.flac":                          "Path references a home directory.",
		"a/\u0301/../x.flac":                 "Path contains a parent-directory reference.",
		"/\u036f0.flac":                      "Path contains an empty component.",
		"a/x.sh":                             "Unsupported file type: .sh",
		"a/noext":                            "Unsupported file type: .",
		strings.Repeat("a/", 32) + "x.flac":  "Path is 33 levels deep.",
		strings.Repeat("a", 256) + "/x.flac": "Path component is too long: " + strings.Repeat("a", 256),
		strings.Repeat("a/", 1600):           "Path is 3200 bytes long.",
	} {
		if _, err := SanitizeAudioRelativePath(raw); err == nil || err.Error() != want {
			t.Errorf("%q: got %v want %q", raw, err, want)
		}
	}
	for raw, want := range map[string]string{
		"Artist/Album/01 Song.flac": "Artist/Album/01 Song.flac",
		`A\B\.x.m4a`:                "A/B/_.x.m4a",
		"a/../\u0301x.flac":         "a/_../\u0301x.flac",
		"a/b:c*?.FLAC/":             "a/bc.FLAC",
		"Cafe\u0301/x.flac":         "Caf\u00e9/x.flac",
	} {
		if got, err := SanitizeAudioRelativePath(raw); err != nil || got != want {
			t.Errorf("%q: got %q, %v want %q", raw, got, err, want)
		}
	}
}

func TestFingerprintAbsentEqualsDefault(t *testing.T) {
	base := FingerprintInput{Title: "Song", Artist: "Artist", TrackNumber: ptr(1)}
	padded := base
	padded.Title, padded.Artist = " Song\n", "Artist\u00a0"
	if Fingerprint(base) != Fingerprint(padded) {
		t.Error("whitespace changed the fingerprint")
	}
	mix := base
	mix.IsMixCompilation = true
	if Fingerprint(base) == Fingerprint(mix) {
		t.Error("mix flag ignored")
	}
	a, b := base, base
	a.SecondaryGenres, b.SecondaryGenres = []string{"IDM", "Ambient"}, []string{"Ambient", "IDM"}
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("secondary genre order matters")
	}
}

func TestImportNames(t *testing.T) {
	for in, want := range map[string]string{
		"a:b": "ab", ":\u0301": ":\u0301", " .x ": "_.x", "|||": "Unknown", ".\u0301x": ".\u0301x",
	} {
		if got := ImportSanitize(in, "Unknown"); got != want {
			t.Errorf("ImportSanitize(%q) = %q want %q", in, got, want)
		}
	}
	taken := map[string]bool{"x.flac": true, "x (1).flac": true, "noext": true}
	exists := func(s string) bool { return taken[s] }
	if got := UniqueName("x.flac", exists); got != "x (2).flac" {
		t.Errorf("got %q", got)
	}
	if got := UniqueName("noext", exists); got != "noext (1)" {
		t.Errorf("got %q", got)
	}
}

func TestSeparator(t *testing.T) {
	for _, c := range []struct {
		display string
		values  []string
		want    string
		ok      bool
	}{
		{" A ; B ", []string{"a", "b"}, " ; ", true},
		{"A & B & C", []string{"A", "B", "C"}, " & ", true},
		{"A feat. B", []string{"A", "B"}, "", false},
		{"X A, B", []string{"A", "B"}, "", false},
		{"AB", []string{"A", "B"}, "", false},
		{"A, B", []string{"A"}, "", false},
		{`A\\B`, []string{"A", "B"}, `\\`, true},
	} {
		got, ok := Separator(c.display, c.values)
		if got != c.want || ok != c.ok {
			t.Errorf("Separator(%q, %q) = %q,%v want %q,%v", c.display, c.values, got, ok, c.want, c.ok)
		}
	}
}

func TestGenreJoin(t *testing.T) {
	if got := GenreJoin(ptr(" Rock "), []string{"rock", "Pop", " ", "pop", "Jazz", "Funk", "Soul"}); got == nil || *got != "Rock ; Pop ; Jazz ; Funk" {
		t.Errorf("got %v", got)
	}
	if got := GenreJoin(nil, []string{"Pop"}); got == nil || *got != "Pop" {
		t.Errorf("got %v", got)
	}
	if got := GenreJoin(ptr(" "), nil); got != nil {
		t.Errorf("got %q", *got)
	}
	p, s := GenreSplit(GenreJoin(ptr("Rock"), []string{"Pop", "Jazz"}))
	if *p != "Rock" || !slices.Equal(s, []string{"Pop", "Jazz"}) {
		t.Errorf("round trip got %q %q", *p, s)
	}
}

func TestTextHelpers(t *testing.T) {
	if !CaseInsensitiveEqual("in/Song.FLAC", "IN/song.flac") || !CaseInsensitiveEqual("STRASSE", "straße") ||
		!CaseInsensitiveEqual("Caf\u00e9", "CAFE\u0301") || CaseInsensitiveEqual("a", "b") {
		t.Error("CaseInsensitiveEqual")
	}
	if got := Trim("\u200b\u3000x\u00a0\n"); got != "x" {
		t.Errorf("Trim got %q", got)
	}
	if got := Trim("\ufeffx"); got != "\ufeffx" {
		t.Errorf("Trim removed U+FEFF: %q", got)
	}
	if got := TrimWhitespace("\t x \n"); got != "x \n" {
		t.Errorf("TrimWhitespace got %q", got)
	}
}

func TestParseDisc(t *testing.T) {
	str := func(n *int) string {
		if n == nil {
			return "nil"
		}
		return strings.Repeat("|", *n)
	}
	for _, c := range []struct {
		num, tot *string
		n, t     *int
	}{
		{ptr("2"), nil, ptr(2), nil},
		{ptr("2/3"), nil, ptr(2), ptr(3)},
		{ptr(" 2 / 3 "), ptr("4"), ptr(2), ptr(4)},
		{ptr("2/3"), ptr("x"), ptr(2), ptr(3)},
		{ptr("2/3"), ptr("0"), ptr(2), nil},
		{ptr("/3"), nil, ptr(3), nil},
		{ptr("2//3"), nil, ptr(2), nil},
		{ptr("0/0"), nil, nil, nil},
		{nil, ptr(" 5 "), nil, ptr(5)},
	} {
		n, tt := ParseDisc(c.num, c.tot)
		if str(n) != str(c.n) || str(tt) != str(c.t) {
			t.Errorf("ParseDisc(%v, %v) = %s, %s", q(c.num), q(c.tot), str(n), str(tt))
		}
	}
}

func TestWireTypes(t *testing.T) {
	var u UUID
	if err := json.Unmarshal([]byte(`"0bc1946d-2f52-4322-bc8b-776377917f74"`), &u); err != nil || u.String() != "0BC1946D-2F52-4322-BC8B-776377917F74" {
		t.Errorf("UUID: %v %s", err, u)
	}
	tm := Time{time.Unix(1_000_000_000, 999_600_000)}
	if got := tm.String(); got != "2001-09-09T01:46:41.000Z" {
		t.Errorf("Time got %s", got)
	}
	var f SyncFilter
	if err := json.Unmarshal([]byte(`{}`), &f); err != nil || !f.IncludePlaylists || f.PlaylistIDAllowlist != nil {
		t.Errorf("filter defaults: %+v", f)
	}
	if err := json.Unmarshal([]byte(`{"playlistIDAllowlist":[]}`), &f); err != nil || f.AllowsPlaylist(u) {
		t.Error("empty allowlist should allow nothing")
	}
	var p Playlist
	if err := json.Unmarshal([]byte(`{"id":"0BC1946D-2F52-4322-BC8B-776377917F74","name":"n","dateCreated":"2018-11-30T15:46:31.000Z","trackPaths":["a.flac","b.mp3"]}`), &p); err != nil ||
		len(p.Entries) != 2 || p.Entries[1].RelativePath != "b.mp3" || p.Entries[0].TrackID != nil {
		t.Errorf("legacy playlist: %v %+v", err, p)
	}
}

func FuzzSanitizeRelativePath(f *testing.F) {
	for _, s := range []string{"Artist/Album/01.flac", "../x", "a/./b", "....//x", `C:\x`, "e\u0301/\uff0e\uff0e", "~x", ".\u0301."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := SanitizeRelativePath(raw)
		if err != nil {
			return
		}
		if utf8.ValidString(raw) && !norm.NFC.IsNormalString(got) {
			t.Errorf("%q -> %q not NFC", raw, got)
		}
		if strings.ContainsRune(got, 0) || strings.HasPrefix(got, "/") || strings.HasPrefix(got, "~") {
			t.Errorf("%q -> %q", raw, got)
		}
		comps := strings.Split(got, "/")
		if len(comps) > MaxDepth {
			t.Errorf("%q -> %d components", raw, len(comps))
		}
		for _, c := range comps {
			if c == "" || c == "." || c == ".." {
				t.Errorf("%q -> %q has component %q", raw, got, c)
			}
		}
	})
}
