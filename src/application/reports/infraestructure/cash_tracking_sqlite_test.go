//go:build sidecar

package infraestructure_test

import (
	"github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	"github.com/google/uuid"
	"slices"
	"testing"
	"time"
)

func TestSQLiteReader_CashTrackingStartsWithFirstRealOpening(t *testing.T) {
	f := setupReaderDB(t)
	reader := reportsInfra.NewSQLiteReader()
	date := func(s string) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, on := range []string{"2026-09-01", "2026-09-05", "2026-09-07"} {
		id := uuid.New()
		at := date(on).Add(12 * time.Hour).UnixMilli()
		if _, err := f.db.Exec(`INSERT INTO payments
    (id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
     concept,balance_pending,payment_date,operator_id)
    VALUES(?,?,1,?,?,?,10000,10000,'cash','other',0,?,?)`,
			id, f.gymID, at, at, "P-"+id.String()[:8], on, f.operatorID); err != nil {
			t.Fatal(err)
		}
	}
	seedSession := func(on string, known bool) {
		at := date(on).UnixMilli()
		status := "open"
		var closedAt any
		if !known {
			status = "closed_unverified"
			closedAt = at
		}
		if _, err := f.db.Exec(`INSERT INTO cash_close_events
   (id,gym_id,version,created_at,updated_at,close_date,calculated_cash,closed_by,
    drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,
    activity_cash,adjusted_after_withdrawal,opened_at,opened_by,closed_at)
   VALUES(?,?,1,?,?,?,0,?,?,'main',?,1,?,0,?,0,0,?,?,?)`,
			uuid.New(), f.gymID, at, at, on, f.operatorID, f.gymID, on, status, known,
			at, f.operatorID, closedAt); err != nil {
			t.Fatal(err)
		}
	}
	read := func(from, to string) reports.CashCountSummary {
		tx, err := f.uow.Query(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		r, err := reader.SumCashCountedBetween(tx, f.gymID, date(from), date(to))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// No cuts existed in older versions. Their absence is not pending work.
	history := read("2026-09-01", "2026-09-07")
	if history.ActiveDays != 3 || history.HistoricalActivityDays != 3 || history.MissingActiveDays != 0 || history.UncoveredActivityDays != 0 {
		t.Fatalf("history: %+v", history)
	}
	// A migrated snapshot must not activate tracking or ask for a past count.
	seedSession("2026-09-01", false)
	legacy := read("2026-09-01", "2026-09-07")
	if legacy.HistoricalSessions != 1 || legacy.TotalSessions != 0 || legacy.ClosedUnverifiedSessions != 0 || legacy.UnknownOpeningSessions != 0 || legacy.LatestSessionID != nil || legacy.UncoveredActivityDays != 0 {
		t.Fatalf("legacy: %+v", legacy)
	}
	// The first real opening starts coverage by operational date, not created_at,
	// and applies to future ranges even when the opening falls outside them.
	seedSession("2026-09-06", true)
	mixed := read("2026-09-01", "2026-09-07")
	if mixed.HistoricalActivityDays != 2 || mixed.HistoricalSessions != 1 || mixed.TotalSessions != 1 || mixed.OpenSessions != 1 || mixed.MissingActiveDays != 1 || mixed.UncoveredActivityDays != 1 {
		t.Fatalf("mixed: %+v", mixed)
	}
	if !slices.Equal(mixed.UncoveredActivityDates, []string{"2026-09-07"}) {
		t.Fatalf("mixed uncovered dates = %#v, want [2026-09-07]", mixed.UncoveredActivityDates)
	}
	if history.UncoveredActivityDates == nil || len(history.UncoveredActivityDates) != 0 {
		t.Fatalf("history uncovered dates = %#v, want empty non-nil slice", history.UncoveredActivityDates)
	}
	later := read("2026-09-07", "2026-09-07")
	if later.HistoricalActivityDays != 0 || later.MissingActiveDays != 1 || later.UncoveredActivityDays != 1 || later.TotalSessions != 0 {
		t.Fatalf("later: %+v", later)
	}
	earlier := read("2026-09-01", "2026-09-05")
	if earlier.HistoricalActivityDays != 2 || earlier.TotalSessions != 0 || earlier.UncoveredActivityDays != 0 {
		t.Fatalf("earlier: %+v", earlier)
	}
	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, date("2026-09-01"), date("2026-09-07"))
	if err != nil || activity != 300 {
		t.Fatalf("full cash activity = %v, err=%v", activity, err)
	}
}

func TestSQLiteReader_CashDifferencesListOnlyCertifiedMismatches(t *testing.T) {
	f := setupReaderDB(t)
	reader := reportsInfra.NewSQLiteReader()
	now := time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC).UnixMilli()
	secondaryID, anaID := uuid.New(), uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_drawers
		(id,gym_id,version,created_at,updated_at,code,name,active,is_main)
		VALUES(?,?,1,?,?,'secondary','Caja 2',1,0)`, secondaryID, f.gymID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, ?, ?, 'ana@t.mx', 'x', 'Ana', 'operator')`, anaID, f.gymID, now, now); err != nil {
		t.Fatal(err)
	}
	type seed struct {
		day                string
		drawerID           uuid.UUID
		code, status       string
		expected, counted  int64
		countedBy          uuid.UUID
		reason, correction any
		adjusted           int
		integrityNote      any
	}
	insert := func(s seed) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var cashLeft, withdrawn, destination, withdrawnAt, withdrawnBy, staleAt any
		if s.status == "withdrawn" {
			cashLeft, withdrawn, destination, withdrawnAt, withdrawnBy = s.counted, 0, "gym_fund", now, f.operatorID
		}
		if s.status == "stale" {
			staleAt = now
		}
		if _, err := f.db.Exec(`INSERT INTO cash_close_events
			(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,discrepancy_reason,closed_by,
			 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
			 cash_left,withdrawn_cash,withdrawal_destination,correction_reason,adjusted_after_withdrawal,integrity_note,
			 opened_at,opened_by,closed_at,reconciled_at,reconciled_by,stale_at,withdrawn_at,withdrawn_by)
			VALUES(?,?,1,?,?,?,?,?,?,?, ?,?,?,1,?,?,1,0, ?,?,?,?,?,?, ?,?,?,?,?,?,?,?)`,
			id, f.gymID, now, now, s.day, s.expected, s.counted, s.reason, f.operatorID,
			s.drawerID, s.code, s.day, s.status, s.expected,
			cashLeft, withdrawn, destination, s.correction, s.adjusted, s.integrityNote,
			now-1000, f.operatorID, now, now, s.countedBy, staleAt, withdrawnAt, withdrawnBy); err != nil {
			t.Fatal(err)
		}
		return id
	}
	shortID := insert(seed{day: "2026-08-24", drawerID: f.gymID, code: "main", status: "withdrawn",
		expected: 75_000, counted: 70_000, countedBy: f.operatorID, reason: "Faltó cambio"})
	surplusID := insert(seed{day: "2026-08-25", drawerID: secondaryID, code: "secondary", status: "reconciled",
		expected: 15_000, counted: 16_000, countedBy: anaID})
	insert(seed{day: "2026-08-25", drawerID: f.gymID, code: "main", status: "reconciled",
		expected: 20_000, counted: 20_000, countedBy: f.operatorID})
	insert(seed{day: "2026-08-23", drawerID: f.gymID, code: "main", status: "stale",
		expected: 12_000, counted: 10_000, countedBy: f.operatorID, correction: "cobro tardío"})
	insert(seed{day: "2026-08-22", drawerID: f.gymID, code: "main", status: "withdrawn",
		expected: 6_000, counted: 5_000, countedBy: f.operatorID, adjusted: 1, integrityNote: "corregido"})

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	got, err := reader.SumCashCountedBetween(tx, f.gymID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.DifferenceSessions) != 2 {
		t.Fatalf("difference sessions = %+v, want surplus and shortage only", got.DifferenceSessions)
	}
	surplus, short := got.DifferenceSessions[0], got.DifferenceSessions[1]
	if surplus.SessionID != surplusID || surplus.Date != "2026-08-25" || surplus.Difference != 10 ||
		surplus.DrawerID == nil || *surplus.DrawerID != secondaryID ||
		surplus.DrawerName == nil || *surplus.DrawerName != "Caja 2" ||
		surplus.CountedByName == nil || *surplus.CountedByName != "Ana" || surplus.Reason != nil {
		t.Fatalf("surplus = %+v", surplus)
	}
	if short.SessionID != shortID || short.Date != "2026-08-24" || short.Difference != -50 || short.Sequence != 1 ||
		short.CountedByName == nil || *short.CountedByName != "Operador" ||
		short.Reason == nil || *short.Reason != "Faltó cambio" {
		t.Fatalf("shortage = %+v", short)
	}
	empty, err := reader.SumCashCountedBetween(tx, f.gymID, from.AddDate(0, -1, 0), from.AddDate(0, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	if empty.DifferenceSessions == nil || len(empty.DifferenceSessions) != 0 {
		t.Fatalf("empty range differences = %#v, want empty non-nil slice", empty.DifferenceSessions)
	}
}
