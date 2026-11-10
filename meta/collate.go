package meta

import (
	"strings"
	"sync"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// digitRuns splits s into alternating runs of ASCII digits and other text.
func digitRuns(s string) []string {
	var runs []string
	start := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || isDigit(s[i]) != isDigit(s[i-1]) {
			runs = append(runs, s[start:i])
			start = i
		}
	}
	return runs
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// collators reuses collators, which are costly to build and not safe to share.
var collators = sync.Pool{New: func() any { return collate.New(language.Und, collate.IgnoreCase, collate.IgnoreWidth) }}

// StandardLess approximates localizedStandardCompare == .orderedAscending:
// root-locale collation ignoring case and width, digit runs compared by
// value, ties broken by code point. It orders presentation only; nothing the
// server decides depends on it.
func StandardLess(a, b string) bool {
	c := collators.Get().(*collate.Collator)
	defer collators.Put(c)
	ra, rb := digitRuns(a), digitRuns(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		x, y := ra[i], rb[i]
		if isDigit(x[0]) && isDigit(y[0]) {
			x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			if len(x) != len(y) {
				return len(x) < len(y)
			}
			if x != y {
				return x < y
			}
			continue
		}
		if r := c.CompareString(x, y); r != 0 {
			return r < 0
		}
	}
	if len(ra) != len(rb) {
		return len(ra) < len(rb)
	}
	return a < b
}
