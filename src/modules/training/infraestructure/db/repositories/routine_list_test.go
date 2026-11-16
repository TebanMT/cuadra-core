//go:build server

package repositories

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	"golang.org/x/text/unicode/norm"
)

func TestRoutineSearchSQLMappingMatchesCaseFoldWithoutReplacementCascades(t *testing.T) {
	if len(routineSearch.expansions) == 0 || len(routineSearch.expansions) > 32 {
		t.Fatal("unexpected expansion count", len(routineSearch.expansions))
	}
	for _, expansion := range routineSearch.expansions {
		if !norm.NFD.IsNormalString(expansion.source) || utf8.RuneCountInString(expansion.source) != 1 || utf8.RuneCountInString(expansion.target) < 2 || routine.FoldSearchCase(expansion.source) != expansion.target || routine.FoldSearchCase(expansion.target) != expansion.target {
			t.Fatal("invalid full fold expansion")
		}
		for _, other := range routineSearch.expansions {
			if strings.Contains(expansion.target, other.source) {
				t.Fatal("SQL replacement could cascade")
			}
		}
	}
	from, to := []rune(routineSearch.foldFrom), []rune(routineSearch.foldTo)
	if len(from) <= len(to) {
		t.Fatal("missing mark removal suffix")
	}
	for _, r := range to {
		if routine.FoldSearchCase(string(r)) != string(r) {
			t.Fatal("single-rune mapping is not folded")
		}
	}
	// Emulate only PostgreSQL translate/replace on independent expected vectors;
	// integration tests exercise the same expression on actual PostgreSQL.
	translate := func(value, from, to string) string {
		a, b := []rune(from), []rune(to)
		mapping := map[rune]rune{}
		for i, r := range a {
			if i < len(b) {
				mapping[r] = b[i]
			} else {
				mapping[r] = -1
			}
		}
		return strings.Map(func(r rune) rune {
			if v, ok := mapping[r]; ok {
				return v
			}
			return r
		}, value)
	}
	for _, vector := range []struct{ raw, want string }{{"Straße STRASSE STRAẞE", "strasse strasse strasse"}, {"ΟΣ ος οσ", "οσ οσ οσ"}, {"ﬃ ﬄ և ԵՒ", "ffi ffl եւ եւ"}, {"Pie\u0301rna\u00a0\u2003Completa", "pierna completa"}, {"Ꭰ ꭰ", "ꭰ ꭰ"}} {
		got := translate(norm.NFD.String(vector.raw), routineSearch.foldFrom, routineSearch.foldTo)
		for _, expansion := range routineSearch.expansions {
			got = strings.ReplaceAll(got, expansion.source, expansion.target)
		}
		got = strings.Join(strings.Fields(translate(got, routineSearch.spaceFrom, routineSearch.spaceTo)), " ")
		if got != vector.want || routine.NormalizeSearch(vector.raw) != vector.want || routine.NormalizeSearch(got) != got {
			t.Fatalf("fold mismatch for %q: %q", vector.raw, got)
		}
	}
}

func TestRoutineSearchSQLMappingCoversEveryUnicodeScalar(t *testing.T) {
	from, to := []rune(routineSearch.foldFrom), []rune(routineSearch.foldTo)
	mapped := map[rune]string{}
	for i, r := range from {
		if i < len(to) {
			mapped[r] = string(to[i])
		} else {
			mapped[r] = ""
		}
	}
	for _, expansion := range routineSearch.expansions {
		source, _ := utf8.DecodeRuneInString(expansion.source)
		mapped[source] = expansion.target
	}
	for r := rune(1); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		source := string(r)
		if !norm.NFD.IsNormalString(source) {
			continue
		}
		want := ""
		if !unicode.Is(unicode.Mn, r) {
			want = routine.FoldSearchCase(source)
		}
		got, exists := mapped[r]
		if !exists {
			got = source
		}
		if got != want || routine.FoldSearchCase(want) != want || !norm.NFD.IsNormalString(want) || strings.ContainsFunc(want, func(r rune) bool { return unicode.Is(unicode.Mn, r) }) {
			t.Fatalf("missing or non-idempotent SQL fold U+%04X: %q != %q", r, got, want)
		}
	}
}

func TestRoutineSearchExpressionKeepsUnicodeRulesAndUserTextInParameters(t *testing.T) {
	value := "Straße '); SELECT unsafe --"
	expression, args := routineSearchExpression(&value)
	if strings.Contains(expression, value) || len(args) < 1 || args[0] != value || strings.Count(expression, "?") != len(args) || strings.Count(expression, "replace(") != len(routineSearch.expansions)+1 {
		t.Fatal("scalar or folding rule interpolated into SQL")
	}
	column, columnArgs := routineSearchExpression(nil)
	if !strings.Contains(column, "normalize(title,NFD)") || strings.Count(column, "?") != len(columnArgs) || len(columnArgs) != len(args)-1 {
		t.Fatal("column/scalar expressions differ")
	}
}

func BenchmarkRoutineSearchTableGeneration(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		value := routineSearchTables()
		if len(value.expansions) == 0 {
			b.Fatal("missing full folds")
		}
	}
	b.ReportMetric(float64(len(routineSearch.expansions)), "expansions")
	b.ReportMetric(float64(len(routineSearch.foldFrom)+len(routineSearch.foldTo)+len(routineSearch.spaceFrom)+len(routineSearch.spaceTo)), "mapping_bytes")
}
