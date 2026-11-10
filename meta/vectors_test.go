package meta

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

type vectors struct {
	Fingerprints []fpCase `json:"fingerprints"`
	Files        []fpCase `json:"files"`
	MultiValue   []struct {
		Name      string          `json:"name"`
		Field     Field           `json:"field"`
		Plural    []string        `json:"plural"`
		Native    []string        `json:"native"`
		MbidCount int             `json:"mbidCount"`
		Known     []string        `json:"known"`
		Decision  json.RawMessage `json:"decision"`
		Resolve   struct {
			Values []string `json:"values"`
			Review *string  `json:"review"`
		} `json:"resolve"`
		StoredCredit struct {
			Value  *string `json:"value"`
			IsList bool    `json:"isList"`
		} `json:"storedCredit"`
		StoredGenres          genres   `json:"storedGenres"`
		GenreSplit            genres   `json:"genreSplit"`
		SplitSafe             []string `json:"splitSafe"`
		SplitAmbiguous        []string `json:"splitAmbiguous"`
		HasAmbiguousSeparator bool     `json:"hasAmbiguousSeparator"`
		ValuesOfStored        []string `json:"valuesOfStored"`
		SplitIdentifiers      []string `json:"splitIdentifiers"`
	} `json:"multiValue"`
	ArtistKeys []struct{ Raw, Key string } `json:"artistKeys"`
	Paths      []struct {
		Raw      string  `json:"raw"`
		Relative pathRes `json:"relative"`
		Audio    pathRes `json:"audio"`
	} `json:"paths"`
	Playlists []struct {
		Wire          string `json:"wire"`
		ContentHash   string `json:"contentHash"`
		ManifestEntry string `json:"manifestEntry"`
	} `json:"playlists"`
	Plans     []struct{ Incoming, Local, Filter string } `json:"plans"`
	Organizer []struct {
		Profile string `json:"profile"`
		Tracks  []struct {
			ID                              UUID
			Path, Title                     string
			Artist, AlbumArtist, Album      *string
			Genre                           *string
			TrackNumber, Year               *int
			Format                          Format
			ArtistIsList, AlbumArtistIsList bool
		} `json:"tracks"`
		Operations []struct {
			ID          UUID
			Destination string
			Status      OpStatus
		} `json:"operations"`
	} `json:"organizer"`
}

type fpCase struct {
	File, Title, Expected, TagFingerprint string
	Artist, AlbumArtist, Album, Genre     *string
	TrackNumber, Year                     *int
	SecondaryGenres                       []string
	IsCompilation                         bool
	IsMixCompilation                      *bool
}

type genres struct {
	Primary   *string  `json:"primary"`
	Secondary []string `json:"secondary"`
}

type pathRes struct {
	OK    *string `json:"ok"`
	Error *string `json:"error"`
}

func loadVectors(t *testing.T) vectors {
	b, err := os.ReadFile("../testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (c fpCase) input() FingerprintInput {
	return FingerprintInput{Title: c.Title, Artist: deref(c.Artist), AlbumArtist: deref(c.AlbumArtist), Album: deref(c.Album),
		Genre: deref(c.Genre), TrackNumber: c.TrackNumber, Year: c.Year, SecondaryGenres: c.SecondaryGenres,
		IsCompilation: c.IsCompilation, IsMixCompilation: c.IsMixCompilation != nil && *c.IsMixCompilation}
}

// same compares string slices, treating nil and empty alike.
func same(a, b []string) bool { return len(a) == 0 && len(b) == 0 || reflect.DeepEqual(a, b) }

func q(s *string) string {
	if s == nil {
		return "nil"
	}
	return fmt.Sprintf("%q", *s)
}

func canonical(t *testing.T, x any) string {
	b, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(v)
	return string(b)
}

func canonicalString(t *testing.T, s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return canonical(t, v)
}

func TestVectorsFingerprints(t *testing.T) {
	v := loadVectors(t)
	for i, c := range v.Fingerprints {
		if got := Fingerprint(c.input()); got != c.Expected {
			t.Errorf("fingerprint %d (%q): got %s want %s", i, c.Title, got, c.Expected)
		}
	}
	for _, c := range v.Files {
		if got := Fingerprint(c.input()); got != c.TagFingerprint {
			t.Errorf("file %s: got %s want %s", c.File, got, c.TagFingerprint)
		}
	}
	t.Logf("%d fingerprints, %d files", len(v.Fingerprints), len(v.Files))
}

func TestVectorsMultiValue(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.MultiValue {
		known := map[string]bool{}
		for _, k := range c.Known {
			known[NFC(ArtistKey(k))] = true
		}
		var d *Decision
		switch string(c.Decision) {
		case "null", "":
		case `"keep"`:
			d = &Decision{Keep: true}
		default:
			d = &Decision{}
			if err := json.Unmarshal(c.Decision, &d.Parts); err != nil {
				t.Fatal(err)
			}
		}
		o := Resolve(c.Field, c.Plural, c.Native, c.MbidCount, func(s string) bool { return known[NFC(ArtistKey(s))] }, d)
		if c.Resolve.Review != nil {
			if o.Review != *c.Resolve.Review {
				t.Errorf("%s: resolve got %+v want review %q", c.Name, o, *c.Resolve.Review)
			}
		} else if o.Review != "" || !same(o.Values, c.Resolve.Values) {
			t.Errorf("%s: resolve got %+v want %q", c.Name, o, c.Resolve.Values)
		}
		val, isList := StoredCredit(c.Plural, c.Native, c.MbidCount)
		if q(val) != q(c.StoredCredit.Value) || isList != c.StoredCredit.IsList {
			t.Errorf("%s: storedCredit got %s,%v want %s,%v", c.Name, q(val), isList, q(c.StoredCredit.Value), c.StoredCredit.IsList)
		}
		p, s := StoredGenres(c.Native)
		if q(p) != q(c.StoredGenres.Primary) || !same(s, c.StoredGenres.Secondary) {
			t.Errorf("%s: storedGenres got %s %q want %s %q", c.Name, q(p), s, q(c.StoredGenres.Primary), c.StoredGenres.Secondary)
		}
		first := ""
		if len(c.Native) > 0 {
			first = c.Native[0]
		}
		p, s = GenreSplit(&first)
		if q(p) != q(c.GenreSplit.Primary) || !same(s, c.GenreSplit.Secondary) {
			t.Errorf("%s: genreSplit got %s %q want %s %q", c.Name, q(p), s, q(c.GenreSplit.Primary), c.GenreSplit.Secondary)
		}
		if got, ok := SplitSafe(first); ok != (c.SplitSafe != nil) || !same(got, c.SplitSafe) {
			t.Errorf("%s: splitSafe got %q,%v want %q", c.Name, got, ok, c.SplitSafe)
		}
		if got := SplitAmbiguous(first); !same(got, c.SplitAmbiguous) {
			t.Errorf("%s: splitAmbiguous got %q want %q", c.Name, got, c.SplitAmbiguous)
		}
		if got := HasAmbiguousSeparator(first); got != c.HasAmbiguousSeparator {
			t.Errorf("%s: hasAmbiguousSeparator got %v", c.Name, got)
		}
		if got := ValuesOfStored(first); !same(got, c.ValuesOfStored) {
			t.Errorf("%s: valuesOfStored got %q want %q", c.Name, got, c.ValuesOfStored)
		}
		if got := SplitIdentifiers(c.Native); !same(got, c.SplitIdentifiers) {
			t.Errorf("%s: splitIdentifiers got %q want %q", c.Name, got, c.SplitIdentifiers)
		}
	}
	t.Logf("%d multiValue cases", len(v.MultiValue))
}

func TestVectorsArtistKeys(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.ArtistKeys {
		if got := ArtistKey(c.Raw); got != c.Key {
			t.Errorf("key(%q): got %q want %q", c.Raw, got, c.Key)
		}
	}
	t.Logf("%d artist keys", len(v.ArtistKeys))
}

func checkPath(t *testing.T, kind, raw string, got string, err error, want pathRes) {
	t.Helper()
	switch {
	case want.OK != nil && (err != nil || got != *want.OK):
		t.Errorf("%s(%q): got %q, %v want %q", kind, raw, got, err, *want.OK)
	case want.Error != nil && (err == nil || err.Error() != *want.Error):
		t.Errorf("%s(%q): got %q, %v want error %q", kind, raw, got, err, *want.Error)
	}
}

func TestVectorsPaths(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Paths {
		got, err := SanitizeRelativePath(c.Raw)
		checkPath(t, "relative", c.Raw, got, err, c.Relative)
		got, err = SanitizeAudioRelativePath(c.Raw)
		checkPath(t, "audio", c.Raw, got, err, c.Audio)
	}
	t.Logf("%d paths", len(v.Paths))
}

func TestVectorsPlaylists(t *testing.T) {
	v := loadVectors(t)
	for i, c := range v.Playlists {
		var p Playlist
		if err := json.Unmarshal([]byte(c.Wire), &p); err != nil {
			t.Fatalf("playlist %d: %v", i, err)
		}
		if got := PlaylistContentHash(p); got != c.ContentHash {
			t.Errorf("playlist %d (%q): hash got %s want %s", i, p.Name, got, c.ContentHash)
		}
		if got, want := canonical(t, PlaylistManifest(p)), canonicalString(t, c.ManifestEntry); got != want {
			t.Errorf("playlist %d: manifest entry\n got %s\nwant %s", i, got, want)
		}
		if got, want := canonical(t, p), canonicalString(t, c.Wire); got != want {
			t.Errorf("playlist %d: round trip\n got %s\nwant %s", i, got, want)
		}
	}
	t.Logf("%d playlists", len(v.Playlists))
}

// TestVectorsPlans round-trips the Mac's manifests and filters: the server
// serves manifests and reads filters; the diff itself runs on clients.
func TestVectorsPlans(t *testing.T) {
	v := loadVectors(t)
	for i, c := range v.Plans {
		var in, local LibraryManifest
		var f SyncFilter
		for _, x := range []struct {
			s string
			p any
		}{{c.Incoming, &in}, {c.Local, &local}, {c.Filter, &f}} {
			if err := json.Unmarshal([]byte(x.s), x.p); err != nil {
				t.Fatalf("plan %d: %v", i, err)
			}
			if got, want := canonical(t, x.p), canonicalString(t, x.s); got != want {
				t.Errorf("plan %d: round trip\n got %s\nwant %s", i, got, want)
			}
		}
	}
	t.Logf("%d plans", len(v.Plans))
}

func TestVectorsOrganizer(t *testing.T) {
	v := loadVectors(t)
	for i, c := range v.Organizer {
		var p Profile
		if err := json.Unmarshal([]byte(c.Profile), &p); err != nil {
			t.Fatal(err)
		}
		tracks := make([]OrganizerTrack, len(c.Tracks))
		credits := make([]Credit, len(c.Tracks))
		for j, x := range c.Tracks {
			tracks[j] = OrganizerTrack(x)
			credits[j] = tracks[j].Credit()
		}
		ops := PlanOrganize(tracks, p, NewArtistResolver(credits, nil))
		if len(ops) != len(c.Operations) {
			t.Fatalf("organizer %d: %d ops want %d", i, len(ops), len(c.Operations))
		}
		for j, want := range c.Operations {
			got := ops[j]
			// The generator recorded String(path.dropFirst("/lib/".count)),
			// which drops Characters: a leading combining mark joins the "/".
			got.Destination = strings.Join(graphemes("/lib/" + got.Destination)[5:], "")
			if got.ID != want.ID || got.Destination != want.Destination || got.Status != want.Status {
				t.Errorf("organizer %d (%s) op %d: got %s %q %s want %s %q %s", i, p.Name, j,
					got.ID, got.Destination, got.Status, want.ID, want.Destination, want.Status)
			}
		}
		if got, want := canonical(t, p), canonicalString(t, c.Profile); got != want {
			t.Errorf("organizer %d: profile round trip\n got %s\nwant %s", i, got, want)
		}
	}
	t.Logf("%d organizer plans", len(v.Organizer))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
