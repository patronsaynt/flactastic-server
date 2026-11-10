package meta

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

const (
	MaxComponentBytes = 255
	MaxDepth          = 32
	MaxTotalBytes     = 3072
)

// Rejection is PathSanitizer.Rejection; Error() is its Swift description.
type Rejection string

func (r Rejection) Error() string { return string(r) }

const (
	ErrEmpty            Rejection = "Path is empty."
	ErrNullByte         Rejection = "Path contains a null byte."
	ErrAbsolute         Rejection = "Path is absolute."
	ErrParentTraversal  Rejection = "Path contains a parent-directory reference."
	ErrHomeReference    Rejection = "Path references a home directory."
	ErrCurrentDirectory Rejection = "Path contains a current-directory reference."
	ErrEmptyComponent   Rejection = "Path contains an empty component."
)

func isAlphabetic(r rune) bool {
	return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_Alphabetic)
}

// SanitizeRelativePath is PathSanitizer.sanitizeRelativePath.
func SanitizeRelativePath(raw string) (string, error) {
	if raw == "" {
		return "", ErrEmpty
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return "", ErrNullByte
	}
	if len(raw) > MaxTotalBytes {
		return "", Rejection(fmt.Sprintf("Path is %d bytes long.", len(raw)))
	}
	n := NFC(raw)
	switch firstGrapheme(n) {
	case "/", "\\":
		return "", ErrAbsolute
	case "~":
		return "", ErrHomeReference
	}
	if uniseg.GraphemeClusterCount(n) >= 2 {
		g := graphemes(n)
		if r, _ := utf8.DecodeRuneInString(g[0]); g[1] == ":" && isAlphabetic(r) {
			return "", ErrAbsolute
		}
	}
	comps := splitGraphemes(n, func(g string) bool { return g == "/" || g == "\\" }, false)
	var safe []string
	for i, c := range comps {
		if c == "" {
			if i == len(comps)-1 {
				continue
			}
			return "", ErrEmptyComponent
		}
		switch {
		case c == ".":
			return "", ErrCurrentDirectory
		case c == "..":
			return "", ErrParentTraversal
		case firstGrapheme(c) == "~":
			return "", ErrHomeReference
		case strings.Trim(c, ".") == "":
			return "", ErrParentTraversal
		case len(c) > MaxComponentBytes:
			return "", Rejection("Path component is too long: " + c)
		}
		safe = append(safe, ImportSanitize(c, "Unknown"))
	}
	if len(safe) == 0 {
		return "", ErrEmpty
	}
	if len(safe) > MaxDepth {
		return "", Rejection(fmt.Sprintf("Path is %d levels deep.", len(safe)))
	}
	path := strings.Join(safe, "/")
	// Swift splits and compares whole Characters, so "/" or "." carrying a
	// combining mark slips through as a non-separator ("../\u0301x" is one
	// component to it) and " ~" trims to "~". Deviation from the Mac: re-check
	// the bytes the filesystem will actually see.
	for _, c := range strings.Split(path, "/") {
		switch {
		case c == "":
			return "", ErrEmptyComponent
		case strings.Trim(c, ".") == "":
			return "", ErrParentTraversal
		case c[0] == '~':
			return "", ErrHomeReference
		}
	}
	return path, nil
}

// SanitizeAudioRelativePath also requires a recognised audio extension.
func SanitizeAudioRelativePath(raw string) (string, error) {
	p, err := SanitizeRelativePath(raw)
	if err != nil {
		return "", err
	}
	ext := pathExt(p)
	if _, ok := ClassifyExt(ext); !ok {
		return "", Rejection("Unsupported file type: ." + ext)
	}
	return p, nil
}

// ImportSanitize is ImportCopy.sanitize: drops the characters
// / \ : * ? " < > | (whole Characters only), trims, and defuses a leading dot.
func ImportSanitize(name, fallback string) string {
	var b strings.Builder
	for _, g := range graphemes(name) {
		if len(g) != 1 || !strings.Contains(`/\:*?"<>|`, g) {
			b.WriteString(g)
		}
	}
	cleaned := Trim(b.String())
	if cleaned == "" {
		return fallback
	}
	if firstGrapheme(cleaned) == "." {
		return "_" + cleaned
	}
	return cleaned
}

// UniqueName is ImportCopy.uniqueDestination's naming rule: filename, else
// "base (1).ext", "base (2).ext", … until exists reports a free name.
func UniqueName(filename string, exists func(string) bool) string {
	ext := pathExt(filename)
	base := filename
	if ext != "" {
		base = filename[:len(filename)-len(ext)-1]
	}
	name := filename
	for i := 1; exists(name); i++ {
		if ext == "" {
			name = fmt.Sprintf("%s (%d)", base, i)
		} else {
			name = fmt.Sprintf("%s (%d).%s", base, i, ext)
		}
	}
	return name
}
