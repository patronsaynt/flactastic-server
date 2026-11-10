package meta

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

// isSpace is Foundation's CharacterSet.whitespaces (CoreFoundation's set:
// note U+200B is in it and U+FEFF is not).
func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == 0xA0 || r == 0x1680 || (r >= 0x2000 && r <= 0x200B) ||
		r == 0x202F || r == 0x205F || r == 0x3000
}

func isNewline(r rune) bool {
	return (r >= 0x0A && r <= 0x0D) || r == 0x85 || r == 0x2028 || r == 0x2029
}

// Trim is trimmingCharacters(in: .whitespacesAndNewlines).
func Trim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return isSpace(r) || isNewline(r) })
}

// TrimWhitespace is trimmingCharacters(in: .whitespaces).
func TrimWhitespace(s string) string { return strings.TrimFunc(s, isSpace) }

// NFC is precomposedStringWithCanonicalMapping.
func NFC(s string) string { return norm.NFC.String(s) }

// Lower is Swift's String.lowercased(): the full, context-free mapping.
func Lower(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == 0x130 {
			b.WriteString("i̇")
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// upper is Swift's String.uppercased().
func upper(s string) string { return cases.Upper(language.Und).String(s) }

// lowerKey is lowercased() as a Swift Set/Dictionary key, where equality is
// canonical equivalence.
func lowerKey(s string) string { return NFC(Lower(s)) }

// less is Swift's String <: NFC scalar order.

// CaseInsensitiveEqual is caseInsensitiveCompare(_:) == .orderedSame:
// full case folding, compared under canonical equivalence.
func CaseInsensitiveEqual(a, b string) bool {
	return NFC(cases.Fold().String(a)) == NFC(cases.Fold().String(b))
}

func graphemes(s string) []string {
	var out []string
	state := -1
	for s != "" {
		var g string
		g, s, _, state = uniseg.FirstGraphemeClusterInString(s, state)
		out = append(out, g)
	}
	return out
}

func firstGrapheme(s string) string {
	g, _, _, _ := uniseg.FirstGraphemeClusterInString(s, -1)
	return g
}

// capGraphemes is String(s.prefix(n)) when s.count > n.
func capGraphemes(s string, n int) string {
	if uniseg.GraphemeClusterCount(s) <= n {
		return s
	}
	return strings.Join(graphemes(s)[:n], "")
}

// splitGraphemes is split(whereSeparator:) on Characters.
func splitGraphemes(s string, sep func(string) bool, omitEmpty bool) []string {
	out := []string{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 || !omitEmpty {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	for _, g := range graphemes(s) {
		if sep(g) {
			flush()
		} else {
			cur.WriteString(g)
		}
	}
	flush()
	return out
}

func runeEqualFold(a, b rune) bool {
	if a == b {
		return true
	}
	for r := unicode.SimpleFold(a); r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return false
}

// foldPrefix reports the byte length of s's prefix matching p case-insensitively.
func foldPrefix(s, p string) (int, bool) {
	if p == "" {
		return 0, false
	}
	j := 0
	for _, pr := range p {
		if j >= len(s) {
			return 0, false
		}
		r, n := utf8.DecodeRuneInString(s[j:])
		if !runeEqualFold(r, pr) {
			return 0, false
		}
		j += n
	}
	return j, true
}

// foldIndex is range(of: sep, options: .caseInsensitive) as byte offsets.
func foldIndex(s, sep string) (int, int) {
	for i := 0; i < len(s); {
		if n, ok := foldPrefix(s[i:], sep); ok {
			return i, i + n
		}
		_, n := utf8.DecodeRuneInString(s[i:])
		i += n
	}
	return -1, -1
}

// pathExt is NSString.pathExtension.
func pathExt(p string) string {
	last := p[strings.LastIndexByte(p, '/')+1:]
	if i := strings.LastIndexByte(last, '.'); i > 0 {
		return last[i+1:]
	}
	return ""
}
