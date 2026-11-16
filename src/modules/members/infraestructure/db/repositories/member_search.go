package repositories

import (
	"strconv"
	"strings"

	"github.com/cuadra/cuadra-core/src/shared/textsearch"
)

// memberSearch es el término del buscador de socios ya interpretado. SQLite
// (sidecar) y Postgres (cloud) arman su WHERE y su ORDER BY con exactamente
// estos valores (vía sql), así recepción y el dashboard encuentran lo mismo.
//
// Reglas:
//   - Nombre: coincide el inicio de CUALQUIER palabra ("gonz" encuentra a
//     "Noe Miranda Gonzales"). Un término con espacio ("juan gar") sigue
//     siendo prefijo de palabra.
//   - Teléfono: substring sobre la versión sin separadores del término.
//   - Término numérico ("1124", "01124", "#01124", "MEM-01124"): además,
//     igualdad numérica contra el folio y el número de socio. Igualdad y no
//     substring, para que "12" no devuelva medio padrón.
type memberSearch struct {
	// namePrefix / nameWord son patrones LIKE escapados (ESCAPE '!'):
	// "term%" = el nombre empieza con el término; "% term%" = alguna palabra
	// posterior empieza con el término.
	namePrefix string
	nameWord   string
	// phoneLike = "%dígitos%" escapado; "" cuando el término no trae dígitos.
	phoneLike string
	// folioNumber: dígitos del término sin ceros a la izquierda, comparados
	// contra la parte numérica del folio (MEM-01124 → "1124"). "" = no aplica.
	folioNumber string
	// memberNumber: número de socio cuando el término es numérico y cabe en
	// INTEGER (Postgres tipa el parámetro como int4). nil = no aplica.
	memberNumber *int
}

// folioNumberSQL extrae la parte numérica del folio sin ceros a la izquierda
// ("MEM-01124" → "1124"). Las funciones (UPPER, REPLACE, LTRIM con set de
// caracteres) existen idénticas en SQLite y Postgres. Un folio no numérico
// (p. ej. "SOC-ab12") nunca es igual a un término de puros dígitos.
const folioNumberSQL = "LTRIM(REPLACE(UPPER(m.folio),'MEM-',''),'0')"

// maxMemberNumberDigits: 9 dígitos siempre caben en int4 (2147483647). Un
// teléfono de 10 dígitos no se compara contra member_number.
const maxMemberNumberDigits = 9

// parseMemberSearch interpreta lo que tecleó el operador. ok=false cuando el
// término está vacío (sin filtro de búsqueda).
func parseMemberSearch(raw string) (memberSearch, bool) {
	s := textsearch.Normalize(raw)
	if s == "" {
		return memberSearch{}, false
	}
	escaped := textsearch.EscapeLike(s)
	out := memberSearch{
		namePrefix: escaped + "%",
		nameWord:   "% " + escaped + "%",
	}
	if phoneDigits := stripPhoneSeparators(s); phoneDigits != "" {
		out.phoneLike = "%" + textsearch.EscapeLike(phoneDigits) + "%"
	}

	compact := strings.TrimPrefix(compactSearchTerm(s), "#")
	digits, folioOnly := "", false
	switch {
	case isAllDigits(compact):
		digits = compact
	case strings.HasPrefix(compact, "mem") && isAllDigits(compact[len("mem"):]):
		// El operador copió el folio tal cual lo muestra la app ("MEM-01124").
		// Es un folio: no lo comparamos contra teléfono ni número de socio.
		digits, folioOnly = compact[len("mem"):], true
	}
	if number := strings.TrimLeft(digits, "0"); number != "" {
		out.folioNumber = number
		if !folioOnly && len(number) <= maxMemberNumberDigits {
			if n, err := strconv.Atoi(number); err == nil {
				out.memberNumber = &n
			}
		}
	}
	return out, true
}

// sql devuelve el filtro (para WHERE) y el rango de relevancia (para el
// inicio del ORDER BY), ambos con placeholders `?` y sus argumentos en orden.
// nameCol es la expresión normalizada del nombre de cada dialecto
// (textsearch.SQLiteColumn / textsearch.PostgresColumn).
//
// Relevancia: 0 = el nombre empieza con el término, 1 = alguna palabra
// empieza con el término, 2 = coincidió por folio, número o teléfono.
func (s memberSearch) sql(nameCol string) (where string, whereArgs []any, rank string, rankArgs []any) {
	conds := []string{
		nameCol + " LIKE ? ESCAPE '!'",
		nameCol + " LIKE ? ESCAPE '!'",
	}
	whereArgs = []any{s.namePrefix, s.nameWord}
	if s.phoneLike != "" {
		conds = append(conds, "m.phone LIKE ? ESCAPE '!'")
		whereArgs = append(whereArgs, s.phoneLike)
	}
	if s.folioNumber != "" {
		conds = append(conds, folioNumberSQL+" = ?")
		whereArgs = append(whereArgs, s.folioNumber)
	}
	if s.memberNumber != nil {
		conds = append(conds, "m.member_number = ?")
		whereArgs = append(whereArgs, *s.memberNumber)
	}
	where = "(" + strings.Join(conds, " OR ") + ")"
	rank = "CASE WHEN " + nameCol + " LIKE ? ESCAPE '!' THEN 0 WHEN " + nameCol + " LIKE ? ESCAPE '!' THEN 1 ELSE 2 END"
	rankArgs = []any{s.namePrefix, s.nameWord}
	return where, whereArgs, rank, rankArgs
}

// compactSearchTerm quita los mismos separadores que stripPhoneSeparators
// ("55 1234", "MEM-01124") sin descartar términos sin dígitos.
func compactSearchTerm(s string) string {
	return strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "", "\t", "").Replace(s)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
