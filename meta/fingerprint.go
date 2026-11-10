package meta

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

type FingerprintInput struct {
	Title, Artist, AlbumArtist, Album, Genre string
	TrackNumber, Year                        *int
	SecondaryGenres                          []string
	IsCompilation, IsMixCompilation          bool
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func fpNormalise(s string) string { return Trim(NFC(s)) }

func optInt(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

// Fingerprint is TagFingerprint.compute. Absent strings equal "" and an
// absent mix flag equals false.
func Fingerprint(in FingerprintInput) string {
	sec := make([]string, len(in.SecondaryGenres))
	for i, g := range in.SecondaryGenres {
		sec[i] = fpNormalise(g)
	}
	sort.Strings(sec)
	comp := "0"
	if in.IsCompilation {
		comp = "1"
	}
	fields := []string{
		fpNormalise(in.Title), fpNormalise(in.Artist), fpNormalise(in.AlbumArtist), fpNormalise(in.Album),
		optInt(in.TrackNumber), fpNormalise(in.Genre), strings.Join(sec, ","), optInt(in.Year), comp,
	}
	if in.IsMixCompilation {
		fields = append(fields, "mix")
	}
	return sha256Hex(strings.Join(fields, "\x1f"))
}
