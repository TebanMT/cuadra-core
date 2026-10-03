//go:build server && integration

package infraestructure_test

import (
	"github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestPostgresReader_CashTrackingStartsWithFirstRealOpening(t *testing.T) {
	f := setupTZFixture(t)
	reader := reportsInfra.NewPostgresReader()
	date := func(s string) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, on := range []string{"2026-09-01", "2026-09-05", "2026-09-07"} {
		id := uuid.New()
		at := date(on).Add(12 * time.Hour)
		if err := f.db.Exec(`INSERT INTO payments
    (id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
     concept,balance_pending,payment_date,operator_id)
    VALUES(?,?,1,?,?,?,100,100,'cash','other',0,?,?)`,
			id, f.gymID, at, at, "P-"+id.String()[:8], on, f.userID).Error; err != nil {
			t.Fatal(err)
		}
	}
	seedSession := func(on string, known bool) {
		at := date(on)
		status := "open"
		var closedAt any
		if !known {
			status = "closed_unverified"
			closedAt = at
		}
		if err := f.db.Exec(`INSERT INTO cash_close_events
   (id,gym_id,version,created_at,updated_at,close_date,calculated_cash,closed_by,
    drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,
    activity_cash,adjusted_after_withdrawal,opened_at,opened_by,closed_at)
   VALUES(?,?,1,?,?,?,0,?,?,'main',?,1,?,0,?,0,false,?,?,?)`,
			uuid.New(), f.gymID, at, at, on, f.userID, f.gymID, on, status, known,
			at, f.userID, closedAt).Error; err != nil {
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
