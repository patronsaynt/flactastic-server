package meta

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type GroupingField string

const (
	GroupAlbumArtist         GroupingField = "albumArtist"
	GroupArtist              GroupingField = "artist"
	GroupAlbum               GroupingField = "album"
	GroupGenre               GroupingField = "genre"
	GroupYear                GroupingField = "year"
	GroupDecade              GroupingField = "decade"
	GroupFormat              GroupingField = "format"
	GroupFirstLetterOfArtist GroupingField = "firstLetterOfArtist"
)

var groupingInfo = map[GroupingField][2]string{
	GroupAlbumArtist:         {"Album Artist", "{albumArtist}"},
	GroupArtist:              {"Artist", "{artist}"},
	GroupAlbum:               {"Album", "{album}"},
	GroupGenre:               {"Genre", "{genre}"},
	GroupYear:                {"Year", "{year}"},
	GroupDecade:              {"Decade", "{decade}s"},
	GroupFormat:              {"Format", "{format}"},
	GroupFirstLetterOfArtist: {"Artist Initial", "{artistInitial}"},
}

func (g GroupingField) DisplayName() string     { return groupingInfo[g][0] }
func (g GroupingField) DefaultTemplate() string { return groupingInfo[g][1] }

type HierarchyLevel struct {
	ID           UUID          `json:"id"`
	GroupBy      GroupingField `json:"groupBy"`
	Name         string        `json:"name"`
	NameTemplate string        `json:"nameTemplate"`
}

func (l *HierarchyLevel) UnmarshalJSON(b []byte) error {
	type plain HierarchyLevel
	var v struct {
		plain
		Name *string `json:"name"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*l = HierarchyLevel(v.plain)
	l.Name = v.GroupBy.DisplayName()
	if v.Name != nil {
		l.Name = *v.Name
	}
	return nil
}

// DisplayLabel is the level's name, or its field's name when blank.
func (l HierarchyLevel) DisplayLabel() string {
	if t := TrimWhitespace(l.Name); t != "" {
		return t
	}
	return l.GroupBy.DisplayName()
}

type Profile struct {
	ID                   UUID             `json:"id"`
	Name                 string           `json:"name"`
	Levels               []HierarchyLevel `json:"levels"`
	FileTemplate         string           `json:"fileTemplate"`
	UsePrimaryArtistOnly bool             `json:"usePrimaryArtistOnly"`
	DeleteEmptyOriginals bool             `json:"deleteEmptyOriginals"`
}

func (p *Profile) UnmarshalJSON(b []byte) error {
	type plain Profile
	v := plain{UsePrimaryArtistOnly: true, DeleteEmptyOriginals: true}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*p = Profile(v)
	return nil
}

func level(g GroupingField, name, tmpl string) HierarchyLevel {
	if tmpl == "" {
		tmpl = g.DefaultTemplate()
	}
	return HierarchyLevel{ID: NewUUID(), GroupBy: g, Name: name, NameTemplate: tmpl}
}

func profile(name string, levels ...HierarchyLevel) Profile {
	return Profile{ID: NewUUID(), Name: name, Levels: levels, FileTemplate: "{track} - {title}",
		UsePrimaryArtistOnly: true, DeleteEmptyOriginals: true}
}

var (
	DefaultProfile = profile("Artist / Album",
		level(GroupAlbumArtist, "Album Artist", ""), level(GroupAlbum, "Album", "{album} ({year})"))
	Presets = []Profile{
		DefaultProfile,
		profile("Genre / Artist / Album",
			level(GroupGenre, "Genre", ""), level(GroupAlbumArtist, "Album Artist", ""), level(GroupAlbum, "Album", "")),
		profile("Year / Album",
			level(GroupYear, "Year", ""), level(GroupAlbum, "Album", "{albumArtist} - {album}")),
	}
)

// OrganizerTrack is what the planner reads from a track. Path is relative to
// the library root, "/"-separated.
type OrganizerTrack struct {
	ID                              UUID
	Path                            string
	Title                           string
	Artist, AlbumArtist, Album      *string
	Genre                           *string
	TrackNumber, Year               *int
	Format                          Format
	ArtistIsList, AlbumArtistIsList bool
}

func (t OrganizerTrack) Credit() Credit {
	return Credit{Artist: t.Artist, AlbumArtist: t.AlbumArtist, ArtistIsList: t.ArtistIsList, AlbumArtistIsList: t.AlbumArtistIsList}
}

var tokenKeys = []string{"artist", "albumArtist", "album", "title", "track", "year", "decade", "genre", "format", "ext", "artistInitial"}

func orDefault(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

func primaryArtist(raw string, r *ArtistResolver) string {
	var pieces []string
	if r != nil {
		pieces = r.Split(raw)
	} else if p, ok := SplitSafe(raw); ok {
		pieces = p
	} else {
		pieces = []string{raw}
	}
	if len(pieces) > 0 {
		raw = pieces[0]
	}
	return Trim(raw)
}

func tokenValue(key string, t OrganizerTrack, primaryOnly bool, r *ArtistResolver) string {
	credit := func(raw string) string {
		if primaryOnly {
			return primaryArtist(raw, r)
		}
		return raw
	}
	switch key {
	case "artist":
		return credit(orDefault(t.Artist, "Unknown Artist"))
	case "albumArtist":
		if t.AlbumArtist != nil {
			return credit(*t.AlbumArtist)
		}
		return credit(orDefault(t.Artist, "Unknown Artist"))
	case "album":
		return orDefault(t.Album, "Unknown Album")
	case "title":
		if t.Title == "" {
			return "Untitled"
		}
		return t.Title
	case "track":
		if t.TrackNumber == nil {
			return "00"
		}
		return fmt.Sprintf("%02d", *t.TrackNumber)
	case "year":
		return optInt(t.Year)
	case "decade":
		if t.Year == nil {
			return ""
		}
		return strconv.Itoa(*t.Year / 10 * 10)
	case "genre":
		return orDefault(t.Genre, "")
	case "format":
		return upper(string(t.Format))
	case "ext":
		return Lower(pathExt(t.Path))
	case "artistInitial":
		name := orDefault(t.AlbumArtist, orDefault(t.Artist, ""))
		if primaryOnly {
			name = primaryArtist(name, r)
		}
		if g := firstGrapheme(TrimWhitespace(name)); g != "" {
			return upper(g)
		}
		return "#"
	}
	return ""
}

// RenderTemplate is OrganizerTemplate.render.
func RenderTemplate(template string, t OrganizerTrack, fallback string, primaryOnly bool, r *ArtistResolver) string {
	for _, k := range tokenKeys {
		template = strings.ReplaceAll(template, "{"+k+"}", tokenValue(k, t, primaryOnly, r))
	}
	return ImportSanitize(template, fallback)
}

type OpStatus string

const (
	OpMove      OpStatus = "move"
	OpUnchanged OpStatus = "unchanged"
	OpConflict  OpStatus = "conflict"
)

// Operation is one planned move; Destination is root-relative.
type Operation struct {
	ID          UUID
	Source      string
	Destination string
	Status      OpStatus
}

// PlanOrganize is OrganizerPlanner.plan, sorted like the Mac's preview.
func PlanOrganize(tracks []OrganizerTrack, p Profile, r *ArtistResolver) []Operation {
	ops := make([]Operation, len(tracks))
	count := map[string]int{}
	for i, t := range tracks {
		parts := make([]string, 0, len(p.Levels)+1)
		for _, l := range p.Levels {
			parts = append(parts, RenderTemplate(l.NameTemplate, t, l.DisplayLabel(), p.UsePrimaryArtistOnly, r))
		}
		name := RenderTemplate(p.FileTemplate, t, "Untitled", p.UsePrimaryArtistOnly, r)
		if ext := pathExt(t.Path); ext != "" {
			name += "." + ext
		}
		// URL.path leaves a NUL percent-encoded.
		dest := strings.ReplaceAll(strings.Join(append(parts, name), "/"), "\x00", "%00")
		ops[i] = Operation{ID: t.ID, Source: t.Path, Destination: dest}
		count[dest]++
	}
	for i := range ops {
		switch {
		case CaseInsensitiveEqual(ops[i].Source, ops[i].Destination):
			ops[i].Status = OpUnchanged
		case count[ops[i].Destination] > 1:
			ops[i].Status = OpConflict
		default:
			ops[i].Status = OpMove
		}
	}
	sort.SliceStable(ops, func(i, j int) bool { return StandardLess(ops[i].Destination, ops[j].Destination) })
	return ops
}

// ParseDisc is DiscTag.parse: DISCNUMBER as "2" or "2/3", with DISCTOTAL
// taking precedence over an inline total. Non-positive values are nil.
func ParseDisc(number, total *string) (num, tot *int) {
	var parts []string
	var cur strings.Builder
	for _, g := range graphemes(orDefault(number, "")) {
		if g == "/" && len(parts) == 0 {
			if cur.Len() > 0 {
				parts = append(parts, cur.String())
			}
			cur.Reset()
		} else {
			cur.WriteString(g)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	atoi := func(s string) *int {
		if n, err := strconv.Atoi(s); err == nil {
			return &n
		}
		return nil
	}
	positive := func(n *int) *int {
		if n != nil && *n > 0 {
			return n
		}
		return nil
	}
	var inline *int
	if len(parts) > 0 {
		num = positive(atoi(TrimWhitespace(parts[0])))
	}
	if len(parts) > 1 {
		inline = atoi(TrimWhitespace(parts[1]))
	}
	t := inline
	if total != nil {
		if n := atoi(TrimWhitespace(*total)); n != nil {
			t = n
		}
	}
	return num, positive(t)
}
