//go:build server && integration

package infraestructure_test

import (
	"github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	"github.com/google/uuid"
	"slices"
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

func TestPostgresReader_CashDifferencesListOnlyCertifiedMismatches(t *testing.T) {
	f := setupTZFixture(t)
	reader := reportsInfra.NewPostgresReader()
	now := time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC)
	secondaryID, anaID := uuid.New(), uuid.New()
	if err := f.db.Exec(`INSERT INTO cash_drawers
		(id,gym_id,version,created_at,updated_at,code,name,active,is_main)
		VALUES(?,?,1,?,?,'secondary','Caja 2',true,false)`, secondaryID, f.gymID, now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec(`INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, ?, ?, ?, 'x', 'Ana', 'operator')`, anaID, f.gymID, now, now,
		"ana-"+anaID.String()[:8]+"@t.mx").Error; err != nil {
		t.Fatal(err)
	}
	type seed struct {
		day                string
		drawerID           uuid.UUID
		code, status       string
		expected, counted  float64
		countedBy          uuid.UUID
		reason, correction any
		adjusted           bool
		integrityNote      any
	}
	insert := func(s seed) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var cashLeft, withdrawn, destination, withdrawnAt, withdrawnBy, staleAt any
		if s.status == "withdrawn" {
			cashLeft, withdrawn, destination, withdrawnAt, withdrawnBy = s.counted, 0, "gym_fund", now, f.userID
		}
		if s.status == "stale" {
			staleAt = now
		}
		if err := f.db.Exec(`INSERT INTO cash_close_events
			(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,discrepancy_reason,closed_by,
			 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
			 cash_left,withdrawn_cash,withdrawal_destination,correction_reason,adjusted_after_withdrawal,integrity_note,
			 opened_at,opened_by,closed_at,reconciled_at,reconciled_by,stale_at,withdrawn_at,withdrawn_by)
			VALUES(?,?,1,?,?,?,?,?,?,?, ?,?,?,1,?,?,true,0, ?,?,?,?,?,?, ?,?,?,?,?,?,?,?)`,
			id, f.gymID, now, now, s.day, s.expected, s.counted, s.reason, f.userID,
			s.drawerID, s.code, s.day, s.status, s.expected,
			cashLeft, withdrawn, destination, s.correction, s.adjusted, s.integrityNote,
			now.Add(-time.Second), f.userID, now, now, s.countedBy, staleAt, withdrawnAt, withdrawnBy).Error; err != nil {
			t.Fatal(err)
		}
		return id
	}
	shortID := insert(seed{day: "2026-08-24", drawerID: f.gymID, code: "main", status: "withdrawn",
		expected: 750, counted: 700, countedBy: f.userID, reason: "Faltó cambio"})
	surplusID := insert(seed{day: "2026-08-25", drawerID: secondaryID, code: "secondary", status: "reconciled",
		expected: 150, counted: 160, countedBy: anaID})
	insert(seed{day: "2026-08-25", drawerID: f.gymID, code: "main", status: "reconciled",
		expected: 200, counted: 200, countedBy: f.userID})
	insert(seed{day: "2026-08-23", drawerID: f.gymID, code: "main", status: "stale",
		expected: 120, counted: 100, countedBy: f.userID, correction: "cobro tardío"})
	insert(seed{day: "2026-08-22", drawerID: f.gymID, code: "main", status: "withdrawn",
		expected: 60, counted: 50, countedBy: f.userID, adjusted: true, integrityNote: "corregido"})

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
		short.CountedByName == nil || short.Reason == nil || *short.Reason != "Faltó cambio" {
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

func TestPostgresReader_CashReviewAndFinishedCountBoundary(t *testing.T) {
	f := setupTZFixture(t)
	reader := reportsInfra.NewPostgresReader()
	day := func(s string, hours float64) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return d.Add(time.Duration(hours * float64(time.Hour)))
	}
	pay := func(on string, amount float64, at time.Time) {
		t.Helper()
		id := uuid.New()
		if err := f.db.Exec(`INSERT INTO payments
    (id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
     concept,balance_pending,payment_date,operator_id)
    VALUES(?,?,1,?,?,?,?,?,'cash','other',0,?,?)`,
			id, f.gymID, at, at, "P-"+id.String()[:8], amount, amount, on, f.userID).Error; err != nil {
			t.Fatal(err)
		}
	}
	type session struct {
		on                       string
		sequence                 int
		status                   string
		opening, activity        float64
		openingKnown             bool
		counted, cashLeft        any
		openedAt, closedAt       time.Time
		reconciledAt, reviewedAt any
		reviewedBy, reviewNote   any
	}
	insert := func(s session) {
		t.Helper()
		var reconciledBy any
		if s.reconciledAt != nil {
			reconciledBy = f.userID
		}
		if err := f.db.Exec(`INSERT INTO cash_close_events
			(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
			 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
			 cash_left,opened_at,opened_by,closed_at,reconciled_at,reconciled_by,reviewed_at,reviewed_by,review_note)
			VALUES(?,?,1,?,?,?,?,?,?, ?,'main',?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?)`,
			uuid.New(), f.gymID, s.closedAt, s.closedAt, s.on, s.opening+s.activity, s.counted, f.userID,
			f.gymID, s.on, s.sequence, s.status, s.opening, s.openingKnown, s.activity,
			s.cashLeft, s.openedAt, f.userID, s.closedAt, s.reconciledAt, reconciledBy,
			s.reviewedAt, s.reviewedBy, s.reviewNote).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Day 1: counted and finished without a withdrawal, then a late sale that
	// the owner reviewed in its own session.
	insert(session{on: "2026-09-10", sequence: 1, status: "reconciled", opening: 80, openingKnown: true,
		counted: 80, cashLeft: 80, openedAt: day("2026-09-10", 8), closedAt: day("2026-09-10", 20),
		reconciledAt: day("2026-09-10", 20)})
	pay("2026-09-10", 20, day("2026-09-10", 21))
	insert(session{on: "2026-09-10", sequence: 2, status: "closed_unverified", opening: 80, activity: 20, openingKnown: true,
		openedAt: day("2026-09-10", 20).Add(time.Millisecond), closedAt: day("2026-09-10", 22),
		reviewedAt: day("2026-09-10", 22), reviewedBy: f.userID, reviewNote: "Cobro después del corte"})
	// Day 2: a review that predates the latest close no longer counts.
	pay("2026-09-11", 30, day("2026-09-11", 12))
	insert(session{on: "2026-09-11", sequence: 1, status: "closed_unverified", activity: 30,
		openedAt: day("2026-09-11", 0), closedAt: day("2026-09-11", 22),
		reviewedAt: day("2026-09-11", 21), reviewedBy: f.userID, reviewNote: "Revisión anterior"})

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first, err := reader.SumCashCountedBetween(tx, f.gymID, day("2026-09-10", 0), day("2026-09-10", 0))
	if err != nil {
		t.Fatal(err)
	}
	if first.StaleSessions != 0 || first.ReconciledSessions != 1 || first.ReviewedSessions != 1 ||
		first.ClosedUnverifiedSessions != 0 || first.UncoveredActivityDays != 0 ||
		first.TotalCloses != 2 || first.CountedCloses != 1 {
		t.Fatalf("finished + reviewed day = %+v", first)
	}
	both, err := reader.SumCashCountedBetween(tx, f.gymID, day("2026-09-10", 0), day("2026-09-11", 0))
	if err != nil {
		t.Fatal(err)
	}
	if both.ReviewedSessions != 1 || both.ClosedUnverifiedSessions != 1 || both.UnknownOpeningSessions != 1 ||
		both.StaleSessions != 0 || both.UncoveredActivityDays != 0 {
		t.Fatalf("lapsed review must request attention again: %+v", both)
	}
}

func TestPostgresReader_OpeningDifferencesAreListedApartFromCounts(t *testing.T) {
	f := setupTZFixture(t)
	reader := reportsInfra.NewPostgresReader()
	at := func(s string, hours int) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return d.Add(time.Duration(hours) * time.Hour)
	}
	open := func(on string, opening float64, expected, note any) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := f.db.Exec(`INSERT INTO cash_close_events
			(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,closed_by,
			 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
			 opened_at,opened_by,opening_expected_cash,opening_note)
			VALUES(?,?,1,?,?,?,?,?, ?,'main',?,1,'open',?,TRUE,0, ?,?,?,?)`,
			id, f.gymID, at(on, 8), at(on, 8), on, opening, f.userID,
			f.gymID, on, opening, at(on, 8), f.userID, expected, note).Error; err != nil {
			t.Fatal(err)
		}
		return id
	}
	short := open("2026-09-21", 450, 500, "Faltaban 50 al llegar")
	open("2026-09-22", 300, 300, nil) // matched the last count
	open("2026-09-23", 200, nil, nil) // nothing to compare with

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := reader.SumCashCountedBetween(tx, f.gymID, at("2026-09-21", 0), at("2026-09-23", 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.OpeningDifferences) != 1 || len(summary.DifferenceSessions) != 0 {
		t.Fatalf("opening differences=%+v count differences=%+v", summary.OpeningDifferences, summary.DifferenceSessions)
	}
	got := summary.OpeningDifferences[0]
	if got.SessionID != short || got.Date != "2026-09-21" || got.Difference != -50 ||
		got.Note == nil || *got.Note != "Faltaban 50 al llegar" || got.OpenedByName == nil {
		t.Fatalf("opening difference = %+v", got)
	}
}

func TestPostgresReader_CountWithoutRecordedOpeningClaimsNoDifference(t *testing.T) {
	f := setupTZFixture(t)
	reader := reportsInfra.NewPostgresReader()
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	insert := func(day string, openingKnown bool, expected, counted float64) {
		t.Helper()
		if err := f.db.Exec(`INSERT INTO cash_close_events
			(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,discrepancy_reason,closed_by,
			 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
			 opened_at,opened_by,closed_at,reconciled_at,reconciled_by)
			VALUES(?,?,1,?,?,?,?,?,'nota',?, ?,'main',?,1,'reconciled',0,?,?, ?,?,?,?,?)`,
			uuid.New(), f.gymID, now, now, day, expected, counted, f.userID,
			f.gymID, day, openingKnown, expected, now.Add(-time.Hour), f.userID, now, now, f.userID).Error; err != nil {
			t.Fatal(err)
		}
	}
	insert("2026-09-24", true, 100, 100) // tracking already started
	// An old client counted $520 against an opening nobody recorded.
	insert("2026-09-25", false, 20, 520)

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := reader.SumCashCountedBetween(tx, f.gymID,
		time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.DifferenceSessions) != 0 || summary.LatestDifference != nil || summary.LatestExpected != nil {
		t.Fatalf("invented difference: list=%+v latest=%v expected=%v",
			summary.DifferenceSessions, summary.LatestDifference, summary.LatestExpected)
	}
	if summary.UnknownOpeningSessions != 1 || summary.LatestCounted == nil || *summary.LatestCounted != 520 {
		t.Fatalf("summary = %+v", summary)
	}
}
