package repositories

import (
	"strings"
	"testing"
)

func TestParseMemberSearch(t *testing.T) {
	num := func(n int) *int { return &n }
	for _, tc := range []struct {
		term       string
		ok         bool
		namePrefix string
		phoneLike  string
		folio      string
		number     *int
	}{
		{term: "   ", ok: false},
		{term: "Gonz", ok: true, namePrefix: "gonz%"},
		{term: "Juan Gar", ok: true, namePrefix: "juan gar%"},
		{term: "%", ok: true, namePrefix: "!%%"},
		{term: "1124", ok: true, namePrefix: "1124%", phoneLike: "%1124%", folio: "1124", number: num(1124)},
		{term: "01124", ok: true, namePrefix: "01124%", phoneLike: "%01124%", folio: "1124", number: num(1124)},
		{term: "#01124", ok: true, namePrefix: "#01124%", phoneLike: "%#01124%", folio: "1124", number: num(1124)},
		// Folio copiado tal cual de la app: sólo folio.
		{term: "MEM-01124", ok: true, namePrefix: "mem-01124%", phoneLike: "%mem01124%", folio: "1124"},
		{term: "#MEM-000012", ok: true, namePrefix: "#mem-000012%", phoneLike: "%#mem000012%", folio: "12"},
		{term: "55 1234-56", ok: true, namePrefix: "55 1234-56%", phoneLike: "%55123456%", folio: "55123456", number: num(55123456)},
		// 10 dígitos (teléfono): no cabe en int4, no se compara con member_number.
		{term: "5512345678", ok: true, namePrefix: "5512345678%", phoneLike: "%5512345678%", folio: "5512345678"},
		// Sólo ceros: no hay número que comparar.
		{term: "000", ok: true, namePrefix: "000%", phoneLike: "%000%"},
		// Con '+' es teléfono, no folio/número.
		{term: "+52 55", ok: true, namePrefix: "+52 55%", phoneLike: "%+5255%"},
		{term: "mem", ok: true, namePrefix: "mem%"},
	} {
		t.Run(tc.term, func(t *testing.T) {
			got, ok := parseMemberSearch(tc.term)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if got.namePrefix != tc.namePrefix || got.nameWord != "% "+tc.namePrefix {
				t.Errorf("name patterns = %q / %q, want %q / %q", got.namePrefix, got.nameWord, tc.namePrefix, "% "+tc.namePrefix)
			}
			if got.phoneLike != tc.phoneLike {
				t.Errorf("phoneLike = %q, want %q", got.phoneLike, tc.phoneLike)
			}
			if got.folioNumber != tc.folio {
				t.Errorf("folioNumber = %q, want %q", got.folioNumber, tc.folio)
			}
			switch {
			case tc.number == nil && got.memberNumber != nil:
				t.Errorf("memberNumber = %d, want nil", *got.memberNumber)
			case tc.number != nil && (got.memberNumber == nil || *got.memberNumber != *tc.number):
				t.Errorf("memberNumber = %v, want %d", got.memberNumber, *tc.number)
			}
		})
	}
}

// TestMemberSearchSQLArgsMatchPlaceholders: el WHERE y el CASE de relevancia
// llevan exactamente un argumento por `?`, en orden. Si se desalinean, SQLite
// enlaza los valores corridos y Postgres truena por tipos.
func TestMemberSearchSQLArgsMatchPlaceholders(t *testing.T) {
	for _, term := range []string{"gonz", "1124", "MEM-01124", "5512345678", "+52 55"} {
		s, _ := parseMemberSearch(term)
		where, whereArgs, rank, rankArgs := s.sql("name_col")
		if n := strings.Count(where, "?"); n != len(whereArgs) {
			t.Errorf("%q: where has %d placeholders, %d args", term, n, len(whereArgs))
		}
		if n := strings.Count(rank, "?"); n != len(rankArgs) {
			t.Errorf("%q: rank has %d placeholders, %d args", term, n, len(rankArgs))
		}
	}
}
