//go:build sidecar

package infraestructure_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
)

// Frontera del día local del gym para las columnas de INSTANTE.
//
// El bug: check-ins, altas y restocks se filtraban y agrupaban por día UTC.
// En CDMX (UTC-6) la medianoche UTC cae a las 6 PM, así que todo lo que
// pasaba de las 6 PM en adelante — el horario PICO del gym — se archivaba
// en el día siguiente. Peor: los ingresos (payment_date, columna DATE) sí
// iban en día local, así que el mismo momento físico quedaba repartido en
// dos días distintos según de qué tabla viniera el dato.
//
// El instante de referencia de toda esta suite: 27-jul-2026 7:30 PM en CDMX,
// que en UTC ya es el 28.
const (
	cdmx           = "America/Mexico_City"
	nocheDel27CDMX = "2026-07-28T01:30:00Z" // 27-jul 19:30 en CDMX
)

func mustInstant(t *testing.T, iso string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatalf("parse %q: %v", iso, err)
	}
	return ts
}

func day(t *testing.T, ymd string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", ymd)
	if err != nil {
		t.Fatalf("parse day %q: %v", ymd, err)
	}
	return d
}

// seedCheckin inserta un check-in permitido en el instante dado.
func (f *readerFixture) seedCheckin(t *testing.T, memberID uuid.UUID, at time.Time) {
	t.Helper()
	ms := at.UTC().UnixMilli()
	if _, err := f.db.Exec(`
		INSERT INTO checkins
		   (id, gym_id, version, created_at, updated_at, member_id, checkin_at, method, result, operator_id)
		 VALUES (?, ?, 1, ?, ?, ?, ?, 'manual', 'allowed_active', ?)`,
		uuid.New(), f.gymID, ms, ms, memberID, ms, f.operatorID); err != nil {
		t.Fatalf("seed checkin: %v", err)
	}
}

// TestCountCheckins_NocheLocalCuentaEnSuDia — un check-in de las 7:30 PM del
// 27 pertenece al 27, aunque en UTC ya sea 28.
func TestCountCheckins_NocheLocalCuentaEnSuDia(t *testing.T) {
	f := setupReaderDB(t)
	ana := f.seedMember(t, "Ana")
	f.seedCheckin(t, ana, mustInstant(t, nocheDel27CDMX))

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	reader := reportsInfra.NewSQLiteReader()

	n, err := reader.CountCheckinsBetween(tx, f.gymID, cdmx, day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("CountCheckinsBetween: %v", err)
	}
	if n != 1 {
		t.Errorf("check-ins del 27 (día local) = %d, want 1", n)
	}

	n, err = reader.CountCheckinsBetween(tx, f.gymID, cdmx, day(t, "2026-07-28"), day(t, "2026-07-28"))
	if err != nil {
		t.Fatalf("CountCheckinsBetween: %v", err)
	}
	if n != 0 {
		t.Errorf("check-ins del 28 (día local) = %d, want 0 — el gym no había abierto", n)
	}
}

// TestCountCheckins_SinZonaMantieneDiaUTC — fail-open: sin zona, el
// comportamiento previo (día UTC) se conserva tal cual.
func TestCountCheckins_SinZonaMantieneDiaUTC(t *testing.T) {
	f := setupReaderDB(t)
	ana := f.seedMember(t, "Ana")
	f.seedCheckin(t, ana, mustInstant(t, nocheDel27CDMX))

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	n, err := reportsInfra.NewSQLiteReader().
		CountCheckinsBetween(tx, f.gymID, "", day(t, "2026-07-28"), day(t, "2026-07-28"))
	if err != nil {
		t.Fatalf("CountCheckinsBetween: %v", err)
	}
	if n != 1 {
		t.Errorf("sin zona el check-in debe caer en el día UTC (28): got %d, want 1", n)
	}
}

// TestCheckinsDailySeries_AgrupaPorDiaLocal — la gráfica pinta la barra en
// el día del gym. Agrupar es distinto de filtrar: hay que clasificar CADA
// fila, no sólo acotar el rango.
func TestCheckinsDailySeries_AgrupaPorDiaLocal(t *testing.T) {
	f := setupReaderDB(t)
	ana := f.seedMember(t, "Ana")
	// Dos del 27 local: uno de mañana (mismo día en UTC) y uno de noche
	// (que en UTC ya es 28). Deben terminar en la MISMA barra.
	f.seedCheckin(t, ana, mustInstant(t, "2026-07-27T15:00:00Z")) // 27-jul 9:00 AM CDMX
	f.seedCheckin(t, ana, mustInstant(t, nocheDel27CDMX))         // 27-jul 7:30 PM CDMX

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewSQLiteReader().
		CheckinsDailySeries(tx, f.gymID, cdmx, day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("CheckinsDailySeries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("series = %+v, want 1 barra (ambos check-ins son del 27 local)", rows)
	}
	if got := rows[0].Date.Format("2006-01-02"); got != "2026-07-27" {
		t.Errorf("barra en %s, want 2026-07-27", got)
	}
	if rows[0].Count != 2 {
		t.Errorf("count = %d, want 2", rows[0].Count)
	}
}

// TestCountNewMembers_AltaNocturnaCuentaEnSuDia — members.created_at también
// es un instante; un alta de las 7:30 PM es del 27.
func TestCountNewMembers_AltaNocturnaCuentaEnSuDia(t *testing.T) {
	f := setupReaderDB(t)
	ms := mustInstant(t, nocheDel27CDMX).UnixMilli()
	id := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO members (id, gym_id, version, created_at, updated_at, folio, full_name, phone, created_by)
		VALUES (?, ?, 1, ?, ?, 'SOC-NOCHE', 'Socio Nocturno', '5512345678', ?)`,
		id, f.gymID, ms, ms, f.operatorID); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	n, err := reportsInfra.NewSQLiteReader().
		CountNewMembersBetween(tx, f.gymID, cdmx, day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("CountNewMembersBetween: %v", err)
	}
	if n != 1 {
		t.Errorf("altas del 27 (día local) = %d, want 1", n)
	}
}
