//go:build server

package repositories

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cuadra/cuadra-core/src/modules/training/domain/search"
	"golang.org/x/text/unicode/norm"
)

// Explicit Unicode tables keep SQL equivalent to NormalizeSearch independent
// of the database collation, without unaccent extensions or stored derivations.
// Removed marks go after all mapped characters, as required by translate().
var trainingSearch = trainingSearchTables()

type trainingSearchExpansion struct{ source, target string }
type trainingSearchMapping struct {
	foldFrom, foldTo, spaceFrom, spaceTo string
	expansions                           []trainingSearchExpansion
}

func trainingSearchTables() trainingSearchMapping {
	var from, to, marks, spaces, replacements strings.Builder
	expansions := []trainingSearchExpansion{}
	// Visit Unicode casing/mark/space ranges only. Folding every scalar at
	// startup would allocate hundreds of MB for uncased characters. The unit
	// coverage audit independently checks every valid scalar for omissions.
	candidates := map[rune]struct{}{}
	for _, table := range []*unicode.RangeTable{unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Uppercase, unicode.Other_Lowercase, unicode.Mn, unicode.White_Space} {
		for _, span := range table.R16 {
			for r := rune(span.Lo); r <= rune(span.Hi); r += rune(span.Stride) {
				candidates[r] = struct{}{}
			}
		}
		for _, span := range table.R32 {
			for r := rune(span.Lo); r <= rune(span.Hi); r += rune(span.Stride) {
				candidates[r] = struct{}{}
			}
		}
	}
	ordered := make([]rune, 0, len(candidates))
	for r := range candidates {
		ordered = append(ordered, r)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, r := range ordered {
		if unicode.Is(unicode.Mn, r) {
			marks.WriteRune(r)
		} else {
			source := string(r)
			target := search.FoldCase(source)
			// SQL starts with NFD. Decomposable sources cannot occur there;
			// including their folds would create unnecessary nested replaces.
			if target != source && norm.NFD.IsNormalString(source) {
				if utf8.RuneCountInString(target) == 1 {
					from.WriteRune(r)
					to.WriteString(target)
				} else {
					expansions = append(expansions, trainingSearchExpansion{source, target})
				}
			}
		}
		if unicode.IsSpace(r) {
			spaces.WriteRune(r)
			replacements.WriteByte(' ')
		}
	}
	return trainingSearchMapping{foldFrom: from.String() + marks.String(), foldTo: to.String(), spaceFrom: spaces.String(), spaceTo: replacements.String(), expansions: expansions}
}

// Only fixed column names or a bound scalar are valid sources. User text never
// becomes a SQL expression.
type searchColumn uint8

const (
	searchTitle searchColumn = iota
	searchName
)

func trainingSearchExpression(column searchColumn, value *string) (string, []any) {
	source := "title"
	if column == searchName {
		source = "name"
	}
	args := []any{}
	if value != nil {
		source = "?::text"
		args = append(args, *value)
	}
	expression := "translate(normalize(" + source + ",NFD),?,?)"
	args = append(args, trainingSearch.foldFrom, trainingSearch.foldTo)
	for _, expansion := range trainingSearch.expansions {
		expression = "replace(" + expression + ",?,?)"
		args = append(args, expansion.source, expansion.target)
	}
	expression = "btrim(regexp_replace(translate(" + expression + ",?,?),' +',' ','g'))"
	args = append(args, trainingSearch.spaceFrom, trainingSearch.spaceTo)
	return expression, args
}
