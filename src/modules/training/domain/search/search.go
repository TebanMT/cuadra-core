// Package search normalizes private search material without changing stored text.
package search

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Search material is separate from the display text. NFD, removal of marks,
// Unicode case folding and whitespace folding have the same order in SQL.
// Lowercasing alone does not equate final sigma or the expansions in ß/ẞ.
func Normalize(value string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(value) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(FoldCase(b.String())), " ")
}

// FoldCase chooses one representative of each simple-fold orbit before
// full folding. x/text v0.34.0 swaps the Cherokee pair U+13A0/U+AB70; feeding
// either spelling through the same representative prevents non-idempotent
// search keys while retaining full folds such as ß -> ss. This is private
// search material, never a transformation of the displayed/stored text.
// SQL builds its exact mappings from this function, without locale rules.
func FoldCase(value string) string {
	canonical := strings.Map(func(r rune) rune {
		representative := r
		for other := unicode.SimpleFold(r); other != r; other = unicode.SimpleFold(other) {
			if other < representative {
				representative = other
			}
		}
		return representative
	}, value)
	return cases.Fold().String(canonical)
}
