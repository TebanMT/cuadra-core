package analytics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ---------------------------------------------------------------------------
// Derivaciones puras
// ---------------------------------------------------------------------------

func TestBuildWaterfall_DescomponeElCambio(t *testing.T) {
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	prev := []MemberMonthlyRate{
		{MemberID: a, TypeName: "Mensual", MonthlyRate: 500},
		{MemberID: b, TypeName: "Mensual", MonthlyRate: 500},
		{MemberID: c, TypeName: "Premium", MonthlyRate: 800},
	}
	now := []MemberMonthlyRate{
		{MemberID: a, TypeName: "Mensual", MonthlyRate: 500},
		{MemberID: c, TypeName: "Mensual", MonthlyRate: 500}, // downgrade −300
		{MemberID: d, TypeName: "Premium", MonthlyRate: 800}, // alta +800
	}
	w := buildWaterfall(prev, now) // b se fue: churn −500

	if w.Starting != 1800 || w.Ending != 1800 {
		t.Errorf("starting/ending = %v/%v, want 1800/1800", w.Starting, w.Ending)
	}
	if w.Churned != -500 {
		t.Errorf("churned = %v, want -500", w.Churned)
	}
	if w.New != 800 {
		t.Errorf("new = %v, want 800", w.New)
	}
	if w.TypeChange != -300 {
		t.Errorf("type_change = %v, want -300", w.TypeChange)
	}
	// La identidad del waterfall: starting + new + type_change + churned = ending.
	if got := w.Starting + w.New + w.TypeChange + w.Churned; got != w.Ending {
		t.Errorf("identidad rota: %v != %v", got, w.Ending)
	}
	if len(w.Transitions) != 1 || w.Transitions[0].FromType != "Premium" ||
		w.Transitions[0].ToType != "Mensual" || w.Transitions[0].Count != 1 ||
		w.Transitions[0].MRRDelta != -300 {
		t.Errorf("transitions = %+v", w.Transitions)
	}
}

func TestScoreAtRisk_SenalesOrdenYLimite(t *testing.T) {
	today := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	expSoon := today.AddDate(0, 0, 3)
	expFar := today.AddDate(0, 0, 60)
	rows := []AtRiskRow{
		// 2 señales (caída + deuda) — arriba aunque pagó menos que "Vence".
		{MemberID: uuid.New(), FullName: "Caída y deuda", Checkins14d: 1, AvgPer14d: 4, Balance: 100, ExpiryDate: &expFar, Paid90d: 300},
		// Sano — fuera de la lista.
		{MemberID: uuid.New(), FullName: "Sano", Checkins14d: 5, AvgPer14d: 4, ExpiryDate: &expFar},
		// 1 señal (vence en 3 días).
		{MemberID: uuid.New(), FullName: "Vence", Checkins14d: 3, AvgPer14d: 3, ExpiryDate: &expSoon, Paid90d: 900},
		// Sin hábito previo (avg < 2): la baja asistencia NO cuenta como caída.
		{MemberID: uuid.New(), FullName: "Nuevo sin hábito", Checkins14d: 0, AvgPer14d: 1, ExpiryDate: &expFar},
	}
	out := scoreAtRisk(rows, today)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (sano y sin-hábito fuera): %+v", len(out), out)
	}
	if out[0].FullName != "Caída y deuda" || out[0].Score != 2 {
		t.Errorf("out[0] = %+v, want Caída y deuda con score 2", out[0])
	}
	if len(out[0].Reasons) != 2 || out[0].Reasons[0] != "asistencia" || out[0].Reasons[1] != "deuda" {
		t.Errorf("reasons = %v", out[0].Reasons)
	}
	if out[1].FullName != "Vence" || out[1].DaysToExpiry == nil || *out[1].DaysToExpiry != 3 {
		t.Errorf("out[1] = %+v, want Vence con 3 días", out[1])
	}
}

func TestCohortsToWire_SoloMesesMaduros(t *testing.T) {
	today := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	rows := []CohortRow{
		{CohortMonth: "2026-06", TypeName: "Mensual", Size: 10, RetM1: 7, RetM2: 5, RetM3: 1},
		{CohortMonth: "2026-07", TypeName: "Mensual", Size: 4, RetM1: 1},
	}
	w := cohortsToWire(rows, today)
	// Junio (edad 2 meses): M1 maduro, M2/M3 no.
	if w[0].M1Pct == nil || *w[0].M1Pct != 70 {
		t.Errorf("jun M1 = %v, want 70", w[0].M1Pct)
	}
	if w[0].M2Pct != nil || w[0].M3Pct != nil {
		t.Errorf("jun M2/M3 deben ser nil: %+v", w[0])
	}
	// Julio (edad 1): nada maduro todavía.
	if w[1].M1Pct != nil {
		t.Errorf("jul M1 debe ser nil: %+v", w[1])
	}
	// KPI Retención M1 = cohorte de hace 2 meses (junio).
	if got := retentionM1(rows, today); got == nil || *got != 70 {
		t.Errorf("retention M1 = %v, want 70", got)
	}
}

func TestBuildProjection_OnlyNextMonthAndNoInventedRate(t *testing.T) {
	today := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	monthlyID, premiumID := uuid.New(), uuid.New()
	due := []RenewalDueRow{
		{TypeID: monthlyID, TypeName: "Mensual", Due: 3, RenewalAmount: 500},
		{TypeID: premiumID, TypeName: "Premium", Due: 1, RenewalAmount: 800},
	}
	renewals := []RenewalRateRow{{TypeID: monthlyID, TypeName: "Mensual anterior", Expirations: 10, Renewed: 8}}

	p := buildProjection(due, renewals, today)
	if p.Complete || p.Total != nil || p.Low != nil || p.High != nil {
		t.Errorf("projection with missing history must be incomplete: %+v", p)
	}
	if p.TargetMonth != "2026-09" || len(p.Rows) != 2 {
		t.Errorf("target/rows = %s/%+v", p.TargetMonth, p.Rows)
	}
	if p.Rows[0].TypeName != "Mensual" || p.Rows[0].Projected == nil || *p.Rows[0].Projected != 1200 {
		t.Errorf("known row = %+v, want 1200", p.Rows[0])
	}
	if p.Rows[1].RenewalPct != nil || p.Rows[1].Projected != nil {
		t.Errorf("unknown Premium history must stay nil: %+v", p.Rows[1])
	}
}

// ---------------------------------------------------------------------------
// Execute (composición) con fakes
// ---------------------------------------------------------------------------

type fakeReader struct {
	ratesNow, ratesPrev []MemberMonthlyRate
	newMembers          int
	cohorts             []CohortRow
	atRisk              []AtRiskRow
	pl                  []PLMonthRow
	gender, genderPrev  []GenderRetentionRow
	renewals            []RenewalRateRow
	renewalsDue         []RenewalDueRow
	deep                ProductsDeep
	roi                 PromotionsROI

	genderActivity []GenderActivityRow
	pyramid        []AgePyramidRow
	noBirthdate    int
	payday         []PaydayRow
	fixedCosts     float64
	fixedMonths    int
	heatmap        []HeatCellRow
	reactivated    int
	tenure         *float64
	tenureN        int

	ratesCalls     int
	retentionCalls int
	ratesErr       error
}

func (f *fakeReader) ActiveMonthlyRates(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) ([]MemberMonthlyRate, error) {
	if f.ratesErr != nil {
		return nil, f.ratesErr
	}
	f.ratesCalls++
	return f.ratesPrev, nil
}
func (f *fakeReader) CurrentMonthlyRates(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) ([]MemberMonthlyRate, error) {
	if f.ratesErr != nil {
		return nil, f.ratesErr
	}
	f.ratesCalls++
	return f.ratesNow, nil
}

func TestOverview_DoesNotTurnQueryFailureIntoZero(t *testing.T) {
	uc := NewOverview(&fakeReader{ratesErr: errors.New("database unavailable")}, fakeUoW{})
	_, err := uc.Execute(context.Background(), OverviewInput{GymID: uuid.New()})
	if err == nil {
		t.Fatal("query failure must surface; zero analytics would be misleading")
	}
	var custom sharedDomain.CustomError
	if !errors.As(err, &custom) || custom.ErrorCode != sharedDomain.CodeUnexpected {
		t.Errorf("err = %#v, want unexpected domain error", err)
	}
}

func TestProductsDeepToWire_ProfitAndMarginRequireCost(t *testing.T) {
	cost := 5.0
	out := productsDeepToWire(ProductsDeep{Rows: []ProductDeepRow{
		{Name: "Agua", Revenue30: 150, COGS30: 50, Units30: 10, AvgCost: &cost},
		{Name: "Sin costo", Revenue30: 80, Units30: 2},
	}}, 0)
	if out.Rows[0].Profit30d == nil || *out.Rows[0].Profit30d != 100 {
		t.Fatalf("profit_30d = %v, want 100", out.Rows[0].Profit30d)
	}
	if out.Rows[0].MarginPct == nil || round2(*out.Rows[0].MarginPct) != 66.67 {
		t.Fatalf("margin_pct = %v, want 66.67", out.Rows[0].MarginPct)
	}
	if out.Rows[1].Profit30d != nil || out.Rows[1].MarginPct != nil {
		t.Fatalf("missing cost must keep profit/margin unknown: %+v", out.Rows[1])
	}
}
func (f *fakeReader) CountNewMembersBetween(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _, _ time.Time) (int, error) {
	return f.newMembers, nil
}
func (f *fakeReader) FirstMembershipCohorts(_ sharedDomain.Transaction, _ uuid.UUID, _ int, _ time.Time) ([]CohortRow, error) {
	return f.cohorts, nil
}
func (f *fakeReader) AtRiskCandidates(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) ([]AtRiskRow, error) {
	return f.atRisk, nil
}
func (f *fakeReader) MonthlyPL(_ sharedDomain.Transaction, _ uuid.UUID, _ int, _ time.Time) ([]PLMonthRow, error) {
	return f.pl, nil
}
func (f *fakeReader) RetentionSince(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) ([]GenderRetentionRow, error) {
	f.retentionCalls++
	if f.retentionCalls == 1 {
		return f.gender, nil
	}
	return f.genderPrev, nil
}
func (f *fakeReader) GenderActivity(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) ([]GenderActivityRow, error) {
	return f.genderActivity, nil
}
func (f *fakeReader) AgePyramid(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) ([]AgePyramidRow, int, error) {
	return f.pyramid, f.noBirthdate, nil
}
func (f *fakeReader) PaydayPattern(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) ([]PaydayRow, error) {
	return f.payday, nil
}
func (f *fakeReader) FixedMonthlyCosts(_ sharedDomain.Transaction, _ uuid.UUID, _ int, _ time.Time) (float64, int, error) {
	return f.fixedCosts, f.fixedMonths, nil
}
func (f *fakeReader) WeeklyHeatmap(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _ time.Time) ([]HeatCellRow, error) {
	return f.heatmap, nil
}
func (f *fakeReader) CountReactivations(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (int, error) {
	return f.reactivated, nil
}
func (f *fakeReader) TenureMonths(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) (*float64, int, error) {
	return f.tenure, f.tenureN, nil
}
func (f *fakeReader) RenewalRatesByType(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) ([]RenewalRateRow, error) {
	return f.renewals, nil
}
func (f *fakeReader) RenewalsDueNextMonth(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) ([]RenewalDueRow, error) {
	return f.renewalsDue, nil
}
func (f *fakeReader) ProductsDeep(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) (ProductsDeep, error) {
	return f.deep, nil
}
func (f *fakeReader) PromotionsROI(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (PromotionsROI, error) {
	return f.roi, nil
}

type fakeTx struct{}

func (fakeTx) Execute(fn func(sharedDomain.Transaction) error) error { return fn(fakeTx{}) }

type fakeUoW struct{}

func (fakeUoW) Begin(context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Commit(sharedDomain.Transaction) error                   { return nil }
func (fakeUoW) Rollback(sharedDomain.Transaction) error                 { return nil }
func (fakeUoW) Query(context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Command(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(fakeTx{})
}

type snapshotSpyUoW struct {
	fakeUoW
	snapshots  int
	queries    int
	inSnapshot bool
}

func (u *snapshotSpyUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	u.queries++
	return fakeTx{}, nil
}

func (u *snapshotSpyUoW) ReadSnapshot(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	u.snapshots++
	u.inSnapshot = true
	defer func() { u.inSnapshot = false }()
	return fn(fakeTx{})
}

func TestOverview_ComposesEveryReadAndTimestampInsideOneSnapshot(t *testing.T) {
	uow := &snapshotSpyUoW{}
	uc := NewOverview(&fakeReader{}, uow)
	uc.Now = func() time.Time {
		if !uow.inSnapshot {
			t.Error("GeneratedAt was calculated outside the read snapshot")
		}
		return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	}
	out, err := uc.Execute(context.Background(), OverviewInput{GymID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if uow.snapshots != 1 || uow.queries != 0 {
		t.Fatalf("snapshots=%d queries=%d, want one snapshot and no loose query", uow.snapshots, uow.queries)
	}
	if out.GeneratedAt.IsZero() {
		t.Fatal("generated_at must identify the snapshot")
	}
}

func TestOverview_ComponeKPIs(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	reader := &fakeReader{
		// Hoy: 2 socios × $500 → MRR 1000, ARPU 500.
		ratesNow: []MemberMonthlyRate{
			{MemberID: a, TypeName: "Mensual", MonthlyRate: 500},
			{MemberID: b, TypeName: "Mensual", MonthlyRate: 500},
		},
		ratesPrev:   []MemberMonthlyRate{{MemberID: a, TypeName: "Mensual", MonthlyRate: 500}},
		newMembers:  3,
		reactivated: 1,
		// Base 20 hace 30d, 16 retenidos → churn 20%, lost 4 (pasa el guard
		// de LTV: base ≥20 y bajas ≥2). Net = 3 + 1 − 4 = 0.
		gender: []GenderRetentionRow{
			{Bucket: "hombre", Base: 12, Retained: 10},
			{Bucket: "mujer", Base: 8, Retained: 6},
		},
		// Mes anterior: base 10, 9 retenidos → churn previo 10%.
		genderPrev: []GenderRetentionRow{{Bucket: "hombre", Base: 10, Retained: 9}},
		genderActivity: []GenderActivityRow{
			// 12 activos hombres con 60 visitas/30d → 60/12/(30/7) ≈ 1.17/sem.
			{Bucket: "hombre", Active: 12, Checkins30d: 60, Spend30d: 2400},
			{Bucket: "mujer", Active: 8, Checkins30d: 48, Spend30d: 2000},
		},
		pl:          []PLMonthRow{{Month: "2026-08", Income: 100, COGS: 15, ProductPurchases: 20, Expenses: 30, Refunds: 10}},
		fixedCosts:  10000,
		fixedMonths: 3,
		tenure:      f64(7.5),
		tenureN:     8,
	}
	uc := NewOverview(reader, fakeUoW{})
	uc.Now = func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) }

	out, err := uc.Execute(context.Background(), OverviewInput{GymID: uuid.New()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.KPIs.MRR != 1000 || out.KPIs.ARPU != 500 || out.KPIs.ActiveNow != 2 {
		t.Errorf("mrr/arpu/active = %v/%v/%d", out.KPIs.MRR, out.KPIs.ARPU, out.KPIs.ActiveNow)
	}
	// Delta de MRR = ending − starting = 1000 − 500.
	if out.KPIs.MRRDelta != 500 {
		t.Errorf("mrr_delta = %v, want 500", out.KPIs.MRRDelta)
	}
	if out.KPIs.ChurnPct == nil || *out.KPIs.ChurnPct != 20 {
		t.Errorf("churn = %v, want 20", out.KPIs.ChurnPct)
	}
	if out.KPIs.ChurnPrevPct == nil || *out.KPIs.ChurnPrevPct != 10 {
		t.Errorf("churn_prev = %v, want 10", out.KPIs.ChurnPrevPct)
	}
	// LTV = ARPU / churn = 500 / 0.20 = 2500 (guard satisfecho).
	if out.KPIs.LTV == nil || *out.KPIs.LTV != 2500 {
		t.Errorf("ltv = %v, want 2500", out.KPIs.LTV)
	}
	if out.KPIs.TenureMonths == nil || *out.KPIs.TenureMonths != 7.5 || out.KPIs.TenureN != 8 {
		t.Errorf("tenure = %+v", out.KPIs)
	}
	// Movimiento: personas primero.
	m := out.Movement
	if m.NewMembers != 3 || m.Reactivated != 1 || m.Lost != 4 || m.Net != 0 {
		t.Errorf("movement = %+v", m)
	}
	// El waterfall vive dentro de Movement: 500 + 500 = 1000.
	if m.MRR.Starting != 500 || m.MRR.New != 500 || m.MRR.Ending != 1000 {
		t.Errorf("movement.mrr = %+v", m.MRR)
	}
	// Resultado simple: net = ingresos − compras − gastos − devoluciones.
	if len(out.PLMonthly) != 1 || out.PLMonthly[0].Net != 40 {
		t.Errorf("pl = %+v", out.PLMonthly)
	}
	// Género a fondo: cruzado con la retención del mismo bucket.
	if len(out.GenderDeep) != 2 {
		t.Fatalf("gender_deep = %+v", out.GenderDeep)
	}
	h := out.GenderDeep[0]
	if h.Bucket != "hombre" || h.Active != 12 || h.Spend30d != 2400 || h.AvgSpend30d != 200 {
		t.Errorf("gender_deep[hombre] = %+v", h)
	}
	if h.VisitsPerWeek != 1.17 {
		t.Errorf("visits/week = %v, want 1.17", h.VisitsPerWeek)
	}
	if h.RetainedPct == nil || *h.RetainedPct != float64(10)/12*100 {
		t.Errorf("retained hombre = %v", h.RetainedPct)
	}
	// Punto de equilibrio: 10000 / 500 = 20 necesarios; activos 2 → −18.
	be := out.Breakeven
	if be.NeededMembers != 20 || be.Delta != -18 || be.MonthsWithData != 3 {
		t.Errorf("breakeven = %+v", be)
	}
	// Payday siempre trae los 31 días (rellenados en 0).
	if len(out.Payday.Days) != 31 {
		t.Errorf("payday days = %d, want 31", len(out.Payday.Days))
	}
}

func TestOverview_MonthlyResultMatchesGoldenFinancialEquation(t *testing.T) {
	reader := &fakeReader{pl: []PLMonthRow{{
		Month:            "2026-08",
		Income:           415,
		ProductPurchases: 50,
		Expenses:         1500,
		Refunds:          0,
	}}}
	uc := NewOverview(reader, fakeUoW{})
	uc.Now = func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }
	out, err := uc.Execute(context.Background(), OverviewInput{GymID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.PLMonthly) != 1 || out.PLMonthly[0].Net != -1135 {
		t.Fatalf("monthly result = %+v, want income 415 - purchases 50 - expenses 1500 - refunds 0 = -1135", out.PLMonthly)
	}
}

func f64(v float64) *float64 { return &v }

func TestOverview_LTVGuard_BaseChica(t *testing.T) {
	// 1 baja sobre base de 10: churn se reporta, LTV NO (explotaría).
	reader := &fakeReader{
		ratesNow: []MemberMonthlyRate{{MemberID: uuid.New(), TypeName: "Mensual", MonthlyRate: 500}},
		gender:   []GenderRetentionRow{{Bucket: "hombre", Base: 10, Retained: 9}},
	}
	uc := NewOverview(reader, fakeUoW{})
	uc.Now = func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) }
	out, err := uc.Execute(context.Background(), OverviewInput{GymID: uuid.New()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.KPIs.ChurnPct == nil || *out.KPIs.ChurnPct != 10 {
		t.Errorf("churn = %v, want 10", out.KPIs.ChurnPct)
	}
	if out.KPIs.LTV != nil {
		t.Errorf("ltv = %v, want nil (guard de muestra chica)", *out.KPIs.LTV)
	}
}

func TestUsageBuckets_ClasificaPorCheckins14d(t *testing.T) {
	rows := []AtRiskRow{
		{Checkins14d: 0},
		{Checkins14d: 1},
		{Checkins14d: 2},
		{Checkins14d: 3},
		{Checkins14d: 5},
		{Checkins14d: 6},
		{Checkins14d: 12},
	}
	f := usageBuckets(rows)
	if f.Actives != 7 || f.None != 1 || f.Sporadic != 2 || f.Regular != 2 || f.Frequent != 2 {
		t.Errorf("buckets = %+v", f)
	}
}
