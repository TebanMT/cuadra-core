package repositories

// Escenario compartido de MemberRepository.List: el mismo padrón y las mismas
// expectativas corren contra SQLite (member_list_sqlite_test.go, sidecar) y
// Postgres (member_list_postgres_integration_test.go, server+integration),
// para que recepción y el dashboard nunca discrepen en búsqueda, filtros u
// orden. Cada dialecto sólo aporta cómo sembrar y cómo listar.

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
)

// scenarioToday es el "hoy" del gym en todo el escenario.
var scenarioToday = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

type scenarioMember struct {
	name         string
	folio        string
	phone        string
	memberNumber *int
	inactive     bool
	deleted      bool
	// membership: "" (sin fila), "active" o "pending_payment".
	membership string
	// expiryDays: vencimiento relativo a scenarioToday (sólo "active").
	expiryDays int
}

func scenarioNum(n int) *int { return &n }

// memberListScenario — el padrón. Los teléfonos evitan "12" y "1124" salvo
// donde el caso lo pide (Ana).
func memberListScenario() []scenarioMember {
	return []scenarioMember{
		{name: "Noe Miranda Gonzales", folio: "MEM-01124", phone: "5530000001", memberNumber: scenarioNum(2001), membership: "active", expiryDays: 10},
		{name: "Gonzalo Pérez", folio: "MEM-000002", phone: "5530000002", memberNumber: scenarioNum(1124), membership: "active", expiryDays: -3},
		{name: "Ana González Ruiz", folio: "MEM-000003", phone: "5591124000", memberNumber: scenarioNum(3001), membership: "pending_payment"},
		{name: "Juan García López", folio: "MEM-000004", phone: "5530000004"},
		{name: "Carla Juárez", folio: "MEM-000120", phone: "5530000005", membership: "active", expiryDays: -40},
		{name: "Luis Inactivo", folio: "MEM-000006", phone: "5530000006", inactive: true, membership: "active", expiryDays: -10},
		{name: "Gonzalo Borrado", folio: "MEM-000007", phone: "5530000007", memberNumber: scenarioNum(1125), deleted: true, membership: "active", expiryDays: -3},
		{name: "Pedro Vigente", folio: "MEM-000008", phone: "5530000008", membership: "active", expiryDays: 0},
		{name: "María Ana Torres", folio: "MEM-000009", phone: "5530000009", membership: "active", expiryDays: 20},
	}
}

// scenarioExpiry devuelve start/expiry ("YYYY-MM-DD") de la membership
// sembrada; expiry vacío = NULL (pending_payment).
func scenarioExpiry(m scenarioMember) (start, expiry string) {
	if m.membership == "active" {
		exp := scenarioToday.AddDate(0, 0, m.expiryDays)
		return exp.AddDate(0, 0, -30).Format("2006-01-02"), exp.Format("2006-01-02")
	}
	return scenarioToday.Format("2006-01-02"), ""
}

type listFunc func(t *testing.T, q memRepo.ListQuery) ([]*memRepo.MemberWithMembership, int)

func scenarioNames(rows []*memRepo.MemberWithMembership) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Member.FullName)
	}
	return out
}

// runMemberListScenario corre todas las expectativas. q base: gym + hoy;
// cada subtest ajusta Search / StatusFilter / Sort.
func runMemberListScenario(t *testing.T, base memRepo.ListQuery, list listFunc) {
	t.Helper()
	base.Today = scenarioToday
	base.Page = 1
	base.PageSize = 200

	t.Run("search", func(t *testing.T) {
		for _, tc := range []struct {
			term string
			desc bool // Sort name DESC: la relevancia debe ganarle al orden pedido
			want []string
		}{
			// Apellido: inicio de cualquier palabra. Relevancia: primero el
			// que EMPIEZA con el término, luego el resto por nombre.
			{term: "gonz", want: []string{"Gonzalo Pérez", "Ana González Ruiz", "Noe Miranda Gonzales"}},
			{term: "GONZÁLEZ", want: []string{"Ana González Ruiz"}},
			{term: "gar", want: []string{"Juan García López"}},
			{term: "lopez", want: []string{"Juan García López"}},
			{term: "juan gar", want: []string{"Juan García López"}},
			{term: "garcia lo", want: []string{"Juan García López"}},
			// "ana" empieza "Ana González" (0) y es palabra en "María Ana"
			// (1): aun con orden DESC por nombre, Ana va primero.
			{term: "ana", desc: true, want: []string{"Ana González Ruiz", "María Ana Torres"}},
			// No es inicio de palabra: no matchea.
			{term: "onzal", want: []string{}},
			// Numérico: folio MEM-01124 (Noe), member_number 1124 (Gonzalo),
			// teléfono 5591124000 (Ana). Todos relevancia 2 → por nombre.
			{term: "1124", want: []string{"Ana González Ruiz", "Gonzalo Pérez", "Noe Miranda Gonzales"}},
			// Con cero a la izquierda: igualdad numérica → folio 01124 y
			// número 1124. El teléfono de Ana no contiene "01124".
			{term: "01124", want: []string{"Gonzalo Pérez", "Noe Miranda Gonzales"}},
			{term: "#01124", want: []string{"Gonzalo Pérez", "Noe Miranda Gonzales"}},
			// Folio tal cual lo muestra la app: sólo folio.
			{term: "MEM-01124", want: []string{"Noe Miranda Gonzales"}},
			{term: "mem 000120", want: []string{"Carla Juárez"}},
			// Folio por igualdad, no substring: "12" no trae al folio 01124
			// ni al 000120; sólo el teléfono de Ana contiene "12".
			{term: "12", want: []string{"Ana González Ruiz"}},
			{term: "120", want: []string{"Carla Juárez"}},
			{term: "0120", want: []string{"Carla Juárez"}},
			// Número de socio.
			{term: "2001", want: []string{"Noe Miranda Gonzales"}},
			// El socio borrado (1125) nunca aparece.
			{term: "1125", want: []string{}},
			// Teléfono completo y con separadores.
			{term: "5530000002", want: []string{"Gonzalo Pérez"}},
			{term: "55 3000 0002", want: []string{"Gonzalo Pérez"}},
		} {
			t.Run(tc.term, func(t *testing.T) {
				q := base
				q.Search = tc.term
				q.Sort = "name"
				q.SortAscending = !tc.desc
				rows, total := list(t, q)
				got := scenarioNames(rows)
				if total != len(tc.want) || strings.Join(got, "|") != strings.Join(tc.want, "|") {
					t.Fatalf("search %q: total=%d got=%v want=%v", tc.term, total, got, tc.want)
				}
			})
		}
	})

	t.Run("search_pagination_uses_same_where", func(t *testing.T) {
		q := base
		q.Search = "gonz"
		q.Sort = "name"
		q.SortAscending = true
		q.PageSize = 1
		var got []string
		for page := 1; page <= 3; page++ {
			q.Page = page
			rows, total := list(t, q)
			if total != 3 || len(rows) != 1 {
				t.Fatalf("page %d: total=%d rows=%d", page, total, len(rows))
			}
			got = append(got, rows[0].Member.FullName)
		}
		if want := "Gonzalo Pérez|Ana González Ruiz|Noe Miranda Gonzales"; strings.Join(got, "|") != want {
			t.Fatalf("pages = %v, want %s", got, want)
		}
	})

	t.Run("status_filters", func(t *testing.T) {
		evaluator := access.New()
		filter := func(t *testing.T, status string) []*memRepo.MemberWithMembership {
			t.Helper()
			q := base
			q.StatusFilter = status
			q.Sort = "name"
			q.SortAscending = true
			rows, total := list(t, q)
			if total != len(rows) {
				t.Fatalf("%s: total=%d rows=%d", status, total, len(rows))
			}
			return rows
		}
		expired := filter(t, "expired")
		lapsed := filter(t, "lapsed")
		unpaid := filter(t, "unpaid_enrollment")
		none := filter(t, "no_membership")

		for _, tc := range []struct {
			name   string
			rows   []*memRepo.MemberWithMembership
			want   []string
			access access.AccessStatus
		}{
			{"lapsed", lapsed, []string{"Carla Juárez", "Gonzalo Pérez"}, access.DeniedExpired},
			{"unpaid_enrollment", unpaid, []string{"Ana González Ruiz"}, access.DeniedUnpaidEnrollment},
			{"no_membership", none, []string{"Juan García López"}, access.DeniedNoMembership},
		} {
			if got := scenarioNames(tc.rows); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
			}
			// Filtro e insignia (access_status) nunca discrepan.
			for _, r := range tc.rows {
				if st := evaluator.Evaluate(r.Member, r.CurrentMembership, scenarioToday); st != tc.access {
					t.Errorf("%s: %s tiene access_status %s, want %s", tc.name, r.Member.FullName, st, tc.access)
				}
			}
		}

		// La suma de los tres desgloses es exactamente "expired".
		union := append(append(scenarioNames(lapsed), scenarioNames(unpaid)...), scenarioNames(none)...)
		sort.Strings(union)
		exp := scenarioNames(expired)
		sort.Strings(exp)
		if len(expired) != len(lapsed)+len(unpaid)+len(none) || strings.Join(union, "|") != strings.Join(exp, "|") {
			t.Fatalf("expired=%v != lapsed+unpaid+no_membership=%v", exp, union)
		}
		// Vence hoy = vigente, no vencido.
		for _, name := range exp {
			if name == "Pedro Vigente" || name == "Luis Inactivo" {
				t.Errorf("%s no debe estar en expired", name)
			}
		}
	})

	t.Run("expiry_sort_nulls_last", func(t *testing.T) {
		withExpiry := []string{"Carla Juárez", "Luis Inactivo", "Gonzalo Pérez", "Pedro Vigente", "Noe Miranda Gonzales", "María Ana Torres"}
		for _, asc := range []bool{true, false} {
			q := base
			q.Sort = "expiry"
			q.SortAscending = asc
			rows, total := list(t, q)
			got := scenarioNames(rows)
			if total != 8 || len(got) != 8 {
				t.Fatalf("asc=%v: total=%d got=%v", asc, total, got)
			}
			want := append([]string{}, withExpiry...)
			if !asc {
				for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
					want[i], want[j] = want[j], want[i]
				}
			}
			if strings.Join(got[:6], "|") != strings.Join(want, "|") {
				t.Errorf("asc=%v: con fecha = %v, want %v", asc, got[:6], want)
			}
			// Sin fecha (pending_payment / sin membership) al final, en
			// ambos sentidos.
			tail := append([]string{}, got[6:]...)
			sort.Strings(tail)
			if strings.Join(tail, "|") != "Ana González Ruiz|Juan García López" {
				t.Errorf("asc=%v: los sin fecha deben ir al final; got %v", asc, got)
			}
		}
	})
}
