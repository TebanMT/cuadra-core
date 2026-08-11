// Package analytics — Plus tier, "por qué y qué sigue" (plan Reports-improve
// fase 3). Vive como paquete aparte de reports para no engordar /reports:
// endpoint propio /api/v1/analytics con PlanGate + RequireOwner, y Reader
// propio SOLO Postgres — el sidecar no lo sirve (la pestaña Análisis es del
// dashboard cloud; el desktop no la monta), así que no hay espejo SQLite
// que mantener.
//
// Cada número responde su pregunta en una frase (criterio de "hecho" de la
// fase 3); las preguntas van como doc-comment en cada struct.
//
// CONVENCIÓN DE PREDICADO: para poder viajar al pasado (churn, waterfall,
// cohortes) la "actividad" de un socio se define por COBERTURA DE FECHAS
// (start_date <= D AND expiry_date >= D, socio no borrado y status='active')
// — no por el status de la membresía, que muta con el tiempo. Puede divergir
// marginalmente del KPI de activos del dashboard (predicado canónico); es el
// trade-off documentado de mirar hacia atrás.
package analytics

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// ---------------------------------------------------------------------------
// Reader — la superficie de queries del tier Plus. Implementación única en
// infraestructure/queries_postgres.go (//go:build server).
// ---------------------------------------------------------------------------

type Reader interface {
	// ActiveMonthlyRates — un row por socio activo en la fecha dada con su
	// tarifa MENSUAL-equivalente (price_snapshot / duration_months, fallback
	// duration_days/30). Base de MRR, waterfall y proyección. NO es cash.
	ActiveMonthlyRates(tx sharedDomain.Transaction, gymID uuid.UUID, onDate time.Time) ([]MemberMonthlyRate, error)
	// CountNewMembersBetween — altas (members.created_at) en [from, to],
	// días locales del gym (tzName vía tz.DayBounds).
	CountNewMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (int, error)
	// FirstMembershipCohorts — cohortes por mes de PRIMERA membresía × tipo,
	// con conteos de retención a 1/2/3 meses del alta individual.
	FirstMembershipCohorts(tx sharedDomain.Transaction, gymID uuid.UUID, monthsBack int, today time.Time) ([]CohortRow, error)
	// AtRiskCandidates — socios activos con señales crudas: check-ins de los
	// últimos 14 días vs su propio promedio (8 semanas previas), vencimiento
	// y deuda; el use case puntúa y filtra.
	AtRiskCandidates(tx sharedDomain.Transaction, gymID uuid.UUID, now, today time.Time) ([]AtRiskRow, error)
	// MonthlyPL — P&L por mes calendario (últimos N): ingresos, COGS (costo
	// promedio all-time × unidades vendidas no reembolsadas), gastos
	// generales y devoluciones. SPEC §9.6.
	MonthlyPL(tx sharedDomain.Transaction, gymID uuid.UUID, monthsBack int, today time.Time) ([]PLMonthRow, error)
	// RetentionSince — socios cubiertos en `since` divididos por género, con
	// cuántos siguen cubiertos hoy. Alimenta churn (suma de buckets) y
	// retención por género.
	RetentionSince(tx sharedDomain.Transaction, gymID uuid.UUID, since, today time.Time) ([]GenderRetentionRow, error)
	// RenewalRatesByType — vencimientos de los últimos 90 días por tipo y
	// cuántos renovaron (membresía nueva en [vencimiento−5, vencimiento+15]).
	RenewalRatesByType(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]RenewalRateRow, error)
	// ProductsDeep — top productos 30d con unidades/revenue/costo promedio +
	// compradores distintos (attach rate).
	ProductsDeep(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (ProductsDeep, error)
	// PromotionsROI — por promoción: usos/descuento 90d + retención de
	// quienes la usaron hace 60–180d vs quienes pagaron precio completo.
	PromotionsROI(tx sharedDomain.Transaction, gymID uuid.UUID, now, today time.Time) (PromotionsROI, error)

	// ── v2 (alcance aprobado 10-ago-2026) ────────────────────────────────
	// GenderActivity — por bucket de género: activos hoy, check-ins de los
	// últimos 30 días (instantes) y gasto de 30 días (payment_date). La
	// retención por género sale de RetentionSince; el use case los cruza.
	GenderActivity(tx sharedDomain.Transaction, gymID uuid.UUID, now, today time.Time) ([]GenderActivityRow, error)
	// AgePyramid — socios ACTIVOS por bucket de edad × género. El segundo
	// retorno es cuántos activos no tienen fecha de nacimiento capturada.
	AgePyramid(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]AgePyramidRow, int, error)
	// PaydayPattern — ingresos (no-refund) por DÍA DEL MES acumulados en la
	// ventana [from, to]. El FE pinta el "efecto quincena".
	PaydayPattern(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]PaydayRow, error)
	// FixedMonthlyCosts — promedio mensual de gastos generales EXCLUYENDO
	// mercadería (eso es costo de producto, no gasto fijo) sobre los últimos
	// N meses COMPLETOS. Retorna (promedio, meses con datos).
	FixedMonthlyCosts(tx sharedDomain.Transaction, gymID uuid.UUID, monthsBack int, today time.Time) (float64, int, error)
	// WeeklyHeatmap — check-ins exitosos de las últimas 8 semanas por día de
	// la semana (1=lunes..7=domingo) × hora LOCAL del gym.
	WeeklyHeatmap(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, now time.Time) ([]HeatCellRow, error)
	// CountReactivations — socios con membresía iniciando en [from, to] que
	// venían de un hueco real (membresía previa vencida >15 días antes y sin
	// cobertura el día anterior al arranque). No son "altas" (el socio ya
	// existía) ni aparecen en el churn (no estaban cubiertos hace 30 días).
	CountReactivations(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error)
	// TenureMonths — mediana de vida (primer inicio → último vencimiento) de
	// los socios que se FUERON en los últimos 12 meses, en meses, + tamaño de
	// la muestra. El use case la anula con muestra chica.
	TenureMonths(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (*float64, int, error)
}

type MemberMonthlyRate struct {
	MemberID    uuid.UUID
	TypeName    string
	MonthlyRate float64
}

type CohortRow struct {
	CohortMonth string // "2026-06"
	TypeName    string
	Size        int
	RetM1       int
	RetM2       int
	RetM3       int
}

type AtRiskRow struct {
	MemberID    uuid.UUID
	FullName    string
	Phone       string
	ExpiryDate  *time.Time
	Checkins14d int
	AvgPer14d   float64
	Balance     float64
	Paid90d     float64
}

// PLMonthRow — "¿cuánto quedó el mes después de mercancía, gastos y
// devoluciones?" Net la calcula el use case.
type PLMonthRow struct {
	Month    string  `json:"month"`
	Income   float64 `json:"income"`
	COGS     float64 `json:"cogs"`
	Expenses float64 `json:"expenses"`
	Refunds  float64 `json:"refunds"`
	Net      float64 `json:"net"`
}

type GenderRetentionRow struct {
	Bucket   string
	Base     int
	Retained int
}

type RenewalRateRow struct {
	TypeName    string
	Expirations int
	Renewed     int
}

type ProductsDeep struct {
	BuyersLast30 int
	Rows         []ProductDeepRow
}

type ProductDeepRow struct {
	ProductID uuid.UUID
	Name      string
	Stock     int
	Units30   int
	Revenue30 float64
	AvgCost   *float64
}

type PromotionsROI struct {
	FullPriceBase     int
	FullPriceRetained int
	Rows              []PromotionROIRow
}

type PromotionROIRow struct {
	PromotionID   uuid.UUID
	Name          string
	Uses90d       int
	Discount90d   float64
	PromoBase     int
	PromoRetained int
}

type GenderActivityRow struct {
	Bucket      string
	Active      int
	Checkins30d int
	Spend30d    float64
}

// AgePyramidRow — un bucket de edad con sus tres conteos de género. Con
// JSON tags porque el wire lo emite tal cual.
type AgePyramidRow struct {
	Bucket         string `json:"bucket"`
	Hombre         int    `json:"hombre"`
	Mujer          int    `json:"mujer"`
	NoEspecificado int    `json:"no_especificado"`
}

// PaydayRow — total cobrado en un día-del-mes (1..31) dentro de la ventana.
type PaydayRow struct {
	Day   int     `json:"day"`
	Total float64 `json:"total"`
}

// HeatCellRow — una celda del mapa de calor semanal (1=lunes..7=domingo,
// hora local 0..23).
type HeatCellRow struct {
	Dow   int `json:"dow"`
	Hour  int `json:"hour"`
	Count int `json:"count"`
}

// ---------------------------------------------------------------------------
// Output (wire) — el controller lo emite tal cual (JSON tags aquí).
// ---------------------------------------------------------------------------

type OverviewOutput struct {
	GeneratedAt time.Time `json:"generated_at"`
	KPIs        KPIsWire  `json:"kpis"`
	// Movement — "¿estoy creciendo o encogiendo?" Altas/reactivados/bajas/
	// neto en PERSONAS (números planos primero) + la misma historia en $
	// (el waterfall de MRR fusionado) + retención M1.
	Movement MovementWire `json:"movement"`
	// AtRisk — "¿a quién le hablo esta semana antes de que se me vaya?"
	AtRisk []AtRiskWire `json:"at_risk"`
	// GenderDeep — "¿quién viene, quién gasta y a quién retengo, por
	// género?" El lente de decisión de equipamiento/adecuaciones.
	GenderDeep []GenderDeepRow `json:"gender_deep"`
	// AgePyramid — "¿de qué edades es mi gym, hombres y mujeres?"
	AgePyramid AgePyramidWire `json:"age_pyramid"`
	// Cohorts — "¿cuánta gente de la que entra se queda, y con qué plan?"
	Cohorts []CohortWire `json:"cohorts"`
	// PLMonthly — "¿cuánto quedó cada mes después de todo?" (SPEC §9.6)
	PLMonthly []PLMonthRow `json:"pl_monthly"`
	// Payday — "¿qué días del mes entra el dinero?" (efecto quincena)
	Payday PaydayWire `json:"payday"`
	// Breakeven — "¿cuántos socios necesito para no perder dinero?"
	Breakeven BreakevenWire `json:"breakeven"`
	// WeeklyHeatmap — "¿cuándo se llena y cuándo está muerto el gym?"
	WeeklyHeatmap []HeatCellRow `json:"weekly_heatmap"`
	// Frequency — "¿cuántos de mis socios de verdad vienen?"
	Frequency FrequencyWire `json:"usage_frequency"`
	// Projection — "¿cuánto entra el mes que viene si todo sigue igual?"
	Projection ProjectionWire `json:"projection"`
	// ProductsDeep — "¿qué producto me deja y cuál me estorba?"
	ProductsDeep ProductsDeepWire `json:"products_deep"`
}

type KPIsWire struct {
	// MRR — "¿cuánto vale mi base de socios al mes?" (mensualidad-
	// equivalente de vigentes, NO cash del mes). MRRDelta = vs hace 30 días
	// (ending − starting del waterfall).
	MRR      float64 `json:"mrr"`
	MRRDelta float64 `json:"mrr_delta"`
	// ARPU — "¿cuánto me deja al mes el socio promedio?"
	ARPU float64 `json:"arpu"`
	// LTV — "¿cuánto me dejará un socio durante toda su vida?" ≈ ARPU/churn.
	// GUARD de honestidad: sólo se emite con base ≥20 socios y ≥2 bajas —
	// con una baja suelta el número explota a cifras absurdas.
	LTV *float64 `json:"ltv"`
	// ChurnPct — "de mis socios de hace 30 días, ¿qué % ya no está?"
	// ChurnPrevPct es la misma medición un mes antes (delta en el FE).
	ChurnPct     *float64 `json:"churn_pct"`
	ChurnPrevPct *float64 `json:"churn_prev_pct"`
	ActiveNow    int      `json:"active_now"`
	// TenureMonths — "¿cuántos meses me dura un socio?" (mediana de vida de
	// los que se fueron en 12 meses; nil con muestra < 5).
	TenureMonths *float64 `json:"tenure_months"`
	TenureN      int      `json:"tenure_n"`
}

// MovementWire — personas primero (números planos), pesos después.
type MovementWire struct {
	NewMembers  int `json:"new_members"`
	Reactivated int `json:"reactivated"`
	Lost        int `json:"lost"`
	// Net = altas + reactivados − bajas (los reactivados no son altas: el
	// socio ya existía; tampoco cuentan en bajas — venían de un hueco).
	Net            int           `json:"net"`
	RetentionM1Pct *float64      `json:"retention_m1_pct"`
	MRR            WaterfallWire `json:"mrr"`
}

// GenderDeepRow — el lente completo por bucket: cuántos son, cuánto
// gastaron (30d), qué tanto vienen y qué % se retiene.
type GenderDeepRow struct {
	Bucket        string   `json:"bucket"`
	Active        int      `json:"active"`
	Spend30d      float64  `json:"spend_30d"`
	AvgSpend30d   float64  `json:"avg_spend_30d"`
	VisitsPerWeek float64  `json:"visits_per_week"`
	RetainedPct   *float64 `json:"retained_pct"`
}

type AgePyramidWire struct {
	Rows        []AgePyramidRow `json:"rows"`
	NoBirthdate int             `json:"no_birthdate"`
}

type PaydayWire struct {
	Days       []PaydayRow `json:"days"`
	WindowDays int         `json:"window_days"`
}

// BreakevenWire — gastos fijos ÷ mensualidad promedio = socios necesarios.
// MonthsWithData = 0 significa "captura tus gastos" (estado pedagógico).
type BreakevenWire struct {
	FixedMonthly   float64 `json:"fixed_monthly"`
	MonthsWithData int     `json:"months_with_data"`
	ARPU           float64 `json:"arpu"`
	NeededMembers  int     `json:"needed_members"`
	Delta          int     `json:"delta"`
}

// FrequencyWire — activos por frecuencia de visita (ventana de 14 días de
// AtRiskCandidates: 0 · 1-2 · 3-5 · 6+ check-ins ≈ nada, <1, 1-2, 3+/sem).
type FrequencyWire struct {
	None     int `json:"none"`
	Sporadic int `json:"sporadic"`
	Regular  int `json:"regular"`
	Frequent int `json:"frequent"`
	Actives  int `json:"actives"`
}

type AtRiskWire struct {
	MemberID       string  `json:"member_id"`
	FullName       string  `json:"full_name"`
	Phone          string  `json:"phone"`
	DaysToExpiry   *int    `json:"days_to_expiry"`
	BalancePending float64 `json:"balance_pending"`
	Checkins14d    int     `json:"checkins_14d"`
	AvgPer14d      float64 `json:"avg_14d"`
	// Paid90d — el valor del socio como multiplicador del riesgo (el
	// leaderboard eliminado de Standard renace aquí).
	Paid90d float64  `json:"paid_90d"`
	Reasons []string `json:"reasons"`
	Score   int      `json:"score"`
}

type CohortWire struct {
	CohortMonth string   `json:"cohort_month"`
	TypeName    string   `json:"type_name"`
	Size        int      `json:"size"`
	M1Pct       *float64 `json:"m1_pct"`
	M2Pct       *float64 `json:"m2_pct"`
	M3Pct       *float64 `json:"m3_pct"`
}

// WaterfallWire — Starting + New + TypeChange + Churned = Ending. Churned
// viaja NEGATIVO. Transitions es la matriz de cambios de tipo (sólo pares
// con movimiento). Vive dentro de Movement (fusión aprobada 10-ago).
type WaterfallWire struct {
	Starting    float64          `json:"starting"`
	New         float64          `json:"new"`
	Churned     float64          `json:"churned"`
	TypeChange  float64          `json:"type_change"`
	Ending      float64          `json:"ending"`
	Transitions []TypeTransition `json:"transitions"`
}

type TypeTransition struct {
	FromType string  `json:"from_type"`
	ToType   string  `json:"to_type"`
	Count    int     `json:"count"`
	MRRDelta float64 `json:"mrr_delta"`
}

// ProjectionWire — activos × prob. de renovación por tipo × mensualidad,
// con sensibilidad de ±5 puntos en la renovación.
type ProjectionWire struct {
	Total float64         `json:"total"`
	Low   float64         `json:"low"`
	High  float64         `json:"high"`
	Rows  []ProjectionRow `json:"rows"`
}

type ProjectionRow struct {
	TypeName    string   `json:"type_name"`
	Active      int      `json:"active"`
	RenewalPct  *float64 `json:"renewal_pct"`
	MonthlyRate float64  `json:"monthly_rate"`
	Projected   float64  `json:"projected"`
}

type ProductsDeepWire struct {
	// AttachRatePct — % de socios activos que compraron producto en 30d.
	AttachRatePct *float64          `json:"attach_rate_pct"`
	Rows          []ProductDeepWire `json:"rows"`
}

type ProductDeepWire struct {
	ProductID   string   `json:"product_id"`
	Name        string   `json:"name"`
	Revenue30d  float64  `json:"revenue_30d"`
	Units30d    int      `json:"units_30d"`
	MarginPct   *float64 `json:"margin_pct"`
	DaysOfStock *float64 `json:"days_of_stock"`
}

type PromotionsROIOutput struct {
	GeneratedAt time.Time `json:"generated_at"`
	// FullPricePct — retención de quienes pagaron membresía SIN promo hace
	// 60–180 días (la vara contra la que se compara cada promo).
	FullPriceBase     int                `json:"full_price_base"`
	FullPriceRetained int                `json:"full_price_retained"`
	FullPricePct      *float64           `json:"full_price_pct"`
	Rows              []PromotionROIWire `json:"rows"`
}

type PromotionROIWire struct {
	PromotionID   string   `json:"promotion_id"`
	Name          string   `json:"name"`
	Uses90d       int      `json:"uses_90d"`
	Discount90d   float64  `json:"discount_90d"`
	PromoBase     int      `json:"promo_base"`
	PromoRetained int      `json:"promo_retained"`
	PromoPct      *float64 `json:"promo_pct"`
}

// ---------------------------------------------------------------------------
// Use case
// ---------------------------------------------------------------------------

const (
	cohortMonthsBack = 6
	plMonthsBack     = 6
	atRiskLimit      = 15
	// riskDropFactor: asistencia 14d < 50% de su propio promedio (y el
	// promedio era ≥2 visitas/14d — sin hábito previo no hay "caída").
	riskDropFactor = 0.5
	riskMinHabit   = 2.0
	riskExpiryDays = 7
	sensitivityPP  = 5.0
	// Guard de honestidad del LTV: con menos base/bajas el número explota.
	ltvMinBase = 20
	ltvMinLost = 2
	// Vida mediana: nil con menos de 5 bajas en 12 meses.
	tenureMinSample = 5
	// Efecto quincena: ventana de acumulación por día-del-mes.
	paydayWindowDays = 90
	// Punto de equilibrio: promedio de gastos fijos sobre N meses completos.
	breakevenMonthsBack = 3
)

type Overview struct {
	Reader Reader
	UoW    sharedDomain.UnitOfWork
	// Gyms (opcional) → ancla "hoy" al día LOCAL del gym. Nil = día UTC.
	Gyms gymRepo.GymRepository
	// Now inyectable para tests. Nil = time.Now().UTC().
	Now func() time.Time
}

func NewOverview(reader Reader, uow sharedDomain.UnitOfWork) *Overview {
	return &Overview{Reader: reader, UoW: uow}
}

func (uc *Overview) WithGyms(g gymRepo.GymRepository) *Overview {
	uc.Gyms = g
	return uc
}

func (uc *Overview) now() time.Time {
	if uc.Now != nil {
		return uc.Now().UTC()
	}
	return time.Now().UTC()
}

// localToday resuelve el día calendario del gym (mismo anclaje que reports).
func (uc *Overview) localTodayAndTZ(tx sharedDomain.Transaction, gymID uuid.UUID, now time.Time) (time.Time, string) {
	fallback := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if uc.Gyms == nil {
		return fallback, ""
	}
	g, err := uc.Gyms.GetByID(tx, gymID)
	if err != nil || g == nil {
		return fallback, ""
	}
	return tz.LocalToday(g.Timezone, now), g.Timezone
}

type OverviewInput struct {
	GymID uuid.UUID
}

func (uc *Overview) Execute(ctx context.Context, in OverviewInput) (*OverviewOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := uc.now()
	today, tzName := uc.localTodayAndTZ(tx, in.GymID, now)
	prev30 := today.AddDate(0, 0, -30)
	prev60 := today.AddDate(0, 0, -60)

	out := &OverviewOutput{
		GeneratedAt:   now,
		AtRisk:        []AtRiskWire{},
		GenderDeep:    []GenderDeepRow{},
		Cohorts:       []CohortWire{},
		PLMonthly:     []PLMonthRow{},
		WeeklyHeatmap: []HeatCellRow{},
	}
	out.AgePyramid.Rows = []AgePyramidRow{}
	out.Payday.Days = []PaydayRow{}
	out.Movement.MRR.Transitions = []TypeTransition{}
	out.Projection.Rows = []ProjectionRow{}
	out.ProductsDeep.Rows = []ProductDeepWire{}

	// Snapshots de tarifa mensual — hoy y hace 30 días. Errores degradan
	// suave a vacío (misma postura por-tarjeta que reports: una sub-query
	// rota no debe tirar la página entera).
	ratesNow, _ := uc.Reader.ActiveMonthlyRates(tx, in.GymID, today)
	ratesPrev, _ := uc.Reader.ActiveMonthlyRates(tx, in.GymID, prev30)

	waterfall := buildWaterfall(ratesPrev, ratesNow)
	mrr := waterfall.Ending
	activeNow := len(ratesNow)

	out.KPIs.MRR = round2(mrr)
	out.KPIs.MRRDelta = round2(waterfall.Ending - waterfall.Starting)
	out.KPIs.ActiveNow = activeNow
	if activeNow > 0 {
		out.KPIs.ARPU = round2(mrr / float64(activeNow))
	}

	// Churn actual + retención por bucket de género (misma query, doble
	// uso) y churn del mes anterior para el delta.
	genderRows, _ := uc.Reader.RetentionSince(tx, in.GymID, prev30, today)
	var churnBase, churnRetained int
	retainedByBucket := map[string]*float64{}
	for _, r := range genderRows {
		churnBase += r.Base
		churnRetained += r.Retained
		retainedByBucket[r.Bucket] = pctOf(r.Retained, r.Base)
	}
	lost := churnBase - churnRetained
	if churnBase > 0 {
		churn := float64(lost) / float64(churnBase) * 100
		out.KPIs.ChurnPct = &churn
		// GUARD de honestidad del LTV: una baja suelta en una base chica
		// dispara el número a cifras absurdas que matan credibilidad.
		if churn > 0 && churnBase >= ltvMinBase && lost >= ltvMinLost {
			ltv := round2(out.KPIs.ARPU / (churn / 100))
			out.KPIs.LTV = &ltv
		}
	}
	prevRows, _ := uc.Reader.RetentionSince(tx, in.GymID, prev60, prev30)
	var pBase, pRet int
	for _, r := range prevRows {
		pBase += r.Base
		pRet += r.Retained
	}
	if pBase > 0 {
		prevChurn := float64(pBase-pRet) / float64(pBase) * 100
		out.KPIs.ChurnPrevPct = &prevChurn
	}

	// Vida mediana del socio (muestra chica → nil, el FE explica).
	tenure, tenureN, _ := uc.Reader.TenureMonths(tx, in.GymID, today)
	out.KPIs.TenureN = tenureN
	if tenure != nil && tenureN >= tenureMinSample {
		t := round2(*tenure)
		out.KPIs.TenureMonths = &t
	}

	// Movimiento del mes — personas primero, pesos después.
	newMembers, _ := uc.Reader.CountNewMembersBetween(tx, in.GymID, tzName, prev30, today)
	reactivated, _ := uc.Reader.CountReactivations(tx, in.GymID, prev30, today)
	out.Movement.NewMembers = newMembers
	out.Movement.Reactivated = reactivated
	out.Movement.Lost = lost
	out.Movement.Net = newMembers + reactivated - lost
	out.Movement.MRR = waterfall

	// Cohortes + retención M1 (vive en Movimiento).
	cohorts, _ := uc.Reader.FirstMembershipCohorts(tx, in.GymID, cohortMonthsBack, today)
	out.Cohorts = cohortsToWire(cohorts, today)
	out.Movement.RetentionM1Pct = retentionM1(cohorts, today)

	// Socios en riesgo + frecuencia de uso: MISMA pasada de candidatos
	// (AtRiskCandidates trae a TODOS los activos con sus check-ins 14d).
	candidates, _ := uc.Reader.AtRiskCandidates(tx, in.GymID, now, today)
	out.AtRisk = scoreAtRisk(candidates, today)
	out.Frequency = usageBuckets(candidates)

	// Género a fondo: actividad + gasto cruzados con la retención.
	activity, _ := uc.Reader.GenderActivity(tx, in.GymID, now, today)
	out.GenderDeep = buildGenderDeep(activity, retainedByBucket)

	// Pirámide de edad × género.
	pyramid, noBirthdate, _ := uc.Reader.AgePyramid(tx, in.GymID, today)
	if pyramid != nil {
		out.AgePyramid.Rows = pyramid
	}
	out.AgePyramid.NoBirthdate = noBirthdate

	// P&L mensual (Resultado mensual en el FE).
	pl, _ := uc.Reader.MonthlyPL(tx, in.GymID, plMonthsBack, today)
	for _, m := range pl {
		m.Net = round2(m.Income - m.COGS - m.Expenses - m.Refunds)
		out.PLMonthly = append(out.PLMonthly, m)
	}

	// Efecto quincena — payment_date acumulado por día del mes, 90 días.
	payday, _ := uc.Reader.PaydayPattern(tx, in.GymID, today.AddDate(0, 0, -(paydayWindowDays-1)), today)
	out.Payday = buildPayday(payday)

	// Punto de equilibrio — gastos fijos ÷ mensualidad promedio.
	fixed, months, _ := uc.Reader.FixedMonthlyCosts(tx, in.GymID, breakevenMonthsBack, today)
	out.Breakeven = buildBreakeven(fixed, months, out.KPIs.ARPU, activeNow)

	// Ocupación semanal (día × hora local, 8 semanas).
	if cells, err := uc.Reader.WeeklyHeatmap(tx, in.GymID, tzName, now); err == nil && cells != nil {
		out.WeeklyHeatmap = cells
	}

	// Proyección explicable + productos a fondo.
	renewals, _ := uc.Reader.RenewalRatesByType(tx, in.GymID, today)
	out.Projection = buildProjection(ratesNow, renewals)
	deep, _ := uc.Reader.ProductsDeep(tx, in.GymID, today)
	out.ProductsDeep = productsDeepToWire(deep, activeNow)

	return out, nil
}

type PromotionsROIQuery struct {
	GymID uuid.UUID
}

// PromotionsROIReport — endpoint aparte porque aterriza en /promotions (con
// candado), no en la pestaña Análisis.
func (uc *Overview) PromotionsROIReport(ctx context.Context, in PromotionsROIQuery) (*PromotionsROIOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := uc.now()
	today, _ := uc.localTodayAndTZ(tx, in.GymID, now)
	roi, err := uc.Reader.PromotionsROI(tx, in.GymID, now, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out := &PromotionsROIOutput{
		GeneratedAt:       now,
		FullPriceBase:     roi.FullPriceBase,
		FullPriceRetained: roi.FullPriceRetained,
		FullPricePct:      pctOf(roi.FullPriceRetained, roi.FullPriceBase),
		Rows:              []PromotionROIWire{},
	}
	for _, r := range roi.Rows {
		out.Rows = append(out.Rows, PromotionROIWire{
			PromotionID:   r.PromotionID.String(),
			Name:          r.Name,
			Uses90d:       r.Uses90d,
			Discount90d:   round2(r.Discount90d),
			PromoBase:     r.PromoBase,
			PromoRetained: r.PromoRetained,
			PromoPct:      pctOf(r.PromoRetained, r.PromoBase),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Derivaciones puras (testeables sin DB)
// ---------------------------------------------------------------------------

// buildWaterfall descompone el cambio de MRR entre dos snapshots:
// Starting + New + TypeChange + Churned(neg) = Ending. La matriz de
// transiciones sólo lista pares de tipos con movimiento real.
func buildWaterfall(prev, now []MemberMonthlyRate) WaterfallWire {
	prevBy := make(map[uuid.UUID]MemberMonthlyRate, len(prev))
	for _, r := range prev {
		prevBy[r.MemberID] = r
	}
	nowBy := make(map[uuid.UUID]MemberMonthlyRate, len(now))
	for _, r := range now {
		nowBy[r.MemberID] = r
	}

	var w WaterfallWire
	transitions := map[[2]string]*TypeTransition{}
	for _, r := range prev {
		w.Starting += r.MonthlyRate
		if _, stays := nowBy[r.MemberID]; !stays {
			w.Churned -= r.MonthlyRate
		}
	}
	for _, r := range now {
		w.Ending += r.MonthlyRate
		p, was := prevBy[r.MemberID]
		if !was {
			w.New += r.MonthlyRate
			continue
		}
		if p.TypeName != r.TypeName {
			key := [2]string{p.TypeName, r.TypeName}
			t := transitions[key]
			if t == nil {
				t = &TypeTransition{FromType: p.TypeName, ToType: r.TypeName}
				transitions[key] = t
			}
			t.Count++
			t.MRRDelta += r.MonthlyRate - p.MonthlyRate
		}
		w.TypeChange += r.MonthlyRate - p.MonthlyRate
	}

	w.Starting = round2(w.Starting)
	w.New = round2(w.New)
	w.Churned = round2(w.Churned)
	w.TypeChange = round2(w.TypeChange)
	w.Ending = round2(w.Ending)
	w.Transitions = []TypeTransition{}
	for _, t := range transitions {
		t.MRRDelta = round2(t.MRRDelta)
		w.Transitions = append(w.Transitions, *t)
	}
	sort.Slice(w.Transitions, func(i, j int) bool {
		return w.Transitions[i].Count > w.Transitions[j].Count
	})
	return w
}

// cohortsToWire convierte conteos a % y anula los meses que la cohorte aún
// no madura (cohorte de junio no tiene M2 hasta septiembre).
func cohortsToWire(rows []CohortRow, today time.Time) []CohortWire {
	out := make([]CohortWire, 0, len(rows))
	for _, r := range rows {
		w := CohortWire{CohortMonth: r.CohortMonth, TypeName: r.TypeName, Size: r.Size}
		age := monthsSince(r.CohortMonth, today)
		if age >= 2 {
			w.M1Pct = pctOf(r.RetM1, r.Size)
		}
		if age >= 3 {
			w.M2Pct = pctOf(r.RetM2, r.Size)
		}
		if age >= 4 {
			w.M3Pct = pctOf(r.RetM3, r.Size)
		}
		out = append(out, w)
	}
	return out
}

// retentionM1 — la cohorte de hace 2 meses (la más reciente con M1 maduro),
// agregada sobre todos los tipos.
func retentionM1(rows []CohortRow, today time.Time) *float64 {
	target := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).
		AddDate(0, -2, 0).Format("2006-01")
	var size, ret int
	for _, r := range rows {
		if r.CohortMonth == target {
			size += r.Size
			ret += r.RetM1
		}
	}
	return pctOf(ret, size)
}

// scoreAtRisk aplica las tres señales (caída de asistencia vs su propio
// promedio, vencimiento ≤7d, deuda), filtra score 0 y ordena por score y
// luego por lo que el socio ha pagado (el multiplicador de valor).
func scoreAtRisk(rows []AtRiskRow, today time.Time) []AtRiskWire {
	out := []AtRiskWire{}
	for _, r := range rows {
		var reasons []string
		if r.AvgPer14d >= riskMinHabit && float64(r.Checkins14d) < riskDropFactor*r.AvgPer14d {
			reasons = append(reasons, "asistencia")
		}
		var daysPtr *int
		if r.ExpiryDate != nil {
			days := int(r.ExpiryDate.Sub(today).Hours() / 24)
			daysPtr = &days
			if days <= riskExpiryDays {
				reasons = append(reasons, "vence")
			}
		}
		if r.Balance > 0 {
			reasons = append(reasons, "deuda")
		}
		if len(reasons) == 0 {
			continue
		}
		out = append(out, AtRiskWire{
			MemberID:       r.MemberID.String(),
			FullName:       r.FullName,
			Phone:          r.Phone,
			DaysToExpiry:   daysPtr,
			BalancePending: round2(r.Balance),
			Checkins14d:    r.Checkins14d,
			AvgPer14d:      round2(r.AvgPer14d),
			Paid90d:        round2(r.Paid90d),
			Reasons:        reasons,
			Score:          len(reasons),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Paid90d > out[j].Paid90d
	})
	if len(out) > atRiskLimit {
		out = out[:atRiskLimit]
	}
	return out
}

// usageBuckets clasifica a TODOS los activos por sus check-ins de 14 días:
// 0 · 1-2 · 3-5 · 6+ ≈ no vienen · <1/sem · 1-2/sem · 3+/sem. Reusa la
// pasada de AtRiskCandidates — cero queries extra.
func usageBuckets(rows []AtRiskRow) FrequencyWire {
	f := FrequencyWire{Actives: len(rows)}
	for _, r := range rows {
		switch {
		case r.Checkins14d == 0:
			f.None++
		case r.Checkins14d <= 2:
			f.Sporadic++
		case r.Checkins14d <= 5:
			f.Regular++
		default:
			f.Frequent++
		}
	}
	return f
}

// buildGenderDeep cruza actividad (activos, visitas, gasto) con la
// retención por bucket. Orden fijo hombre/mujer/no_especificado; buckets
// sin activos no se emiten.
func buildGenderDeep(activity []GenderActivityRow, retained map[string]*float64) []GenderDeepRow {
	byBucket := map[string]GenderActivityRow{}
	for _, a := range activity {
		byBucket[a.Bucket] = a
	}
	out := []GenderDeepRow{}
	for _, bucket := range []string{"hombre", "mujer", "no_especificado"} {
		a, ok := byBucket[bucket]
		if !ok || a.Active == 0 {
			continue
		}
		out = append(out, GenderDeepRow{
			Bucket:      bucket,
			Active:      a.Active,
			Spend30d:    round2(a.Spend30d),
			AvgSpend30d: round2(a.Spend30d / float64(a.Active)),
			// 30 días ≈ 30/7 semanas.
			VisitsPerWeek: round2(float64(a.Checkins30d) / float64(a.Active) / (30.0 / 7.0)),
			RetainedPct:   retained[bucket],
		})
	}
	return out
}

// buildPayday rellena los 31 días (los que no tuvieron cobros van en 0
// para que el FE pinte la grilla completa sin reconciliar huecos).
func buildPayday(rows []PaydayRow) PaydayWire {
	byDay := map[int]float64{}
	for _, r := range rows {
		byDay[r.Day] += r.Total
	}
	w := PaydayWire{Days: make([]PaydayRow, 0, 31), WindowDays: paydayWindowDays}
	for d := 1; d <= 31; d++ {
		w.Days = append(w.Days, PaydayRow{Day: d, Total: round2(byDay[d])})
	}
	return w
}

// buildBreakeven — ceil(gastos fijos / ARPU). Sin gastos capturados o sin
// ARPU, NeededMembers queda en 0 y el FE muestra el estado pedagógico.
func buildBreakeven(fixed float64, months int, arpu float64, active int) BreakevenWire {
	w := BreakevenWire{
		FixedMonthly:   round2(fixed),
		MonthsWithData: months,
		ARPU:           arpu,
	}
	if months == 0 || arpu <= 0 || fixed <= 0 {
		return w
	}
	w.NeededMembers = int(math.Ceil(fixed / arpu))
	w.Delta = active - w.NeededMembers
	return w
}

// buildProjection — activos × prob. renovación (por tipo, fallback a la
// global) × tarifa mensual promedio del tipo. Sin historial de renovaciones
// la prob. es 1.0 y RenewalPct viaja nil ("sin historial" en el FE).
func buildProjection(ratesNow []MemberMonthlyRate, renewals []RenewalRateRow) ProjectionWire {
	type agg struct {
		n    int
		rate float64
	}
	byType := map[string]*agg{}
	for _, r := range ratesNow {
		a := byType[r.TypeName]
		if a == nil {
			a = &agg{}
			byType[r.TypeName] = a
		}
		a.n++
		a.rate += r.MonthlyRate
	}

	renewalBy := map[string]RenewalRateRow{}
	var gExp, gRen int
	for _, r := range renewals {
		renewalBy[r.TypeName] = r
		gExp += r.Expirations
		gRen += r.Renewed
	}
	globalPct := pctOf(gRen, gExp)

	p := ProjectionWire{Rows: []ProjectionRow{}}
	var total, low, high float64
	for name, a := range byType {
		avgRate := a.rate / float64(a.n)
		var pct *float64
		if r, ok := renewalBy[name]; ok && r.Expirations > 0 {
			pct = pctOf(r.Renewed, r.Expirations)
		} else {
			pct = globalPct
		}
		prob := 1.0
		if pct != nil {
			prob = *pct / 100
		}
		projected := float64(a.n) * prob * avgRate
		total += projected
		low += float64(a.n) * clamp01(prob-sensitivityPP/100) * avgRate
		high += float64(a.n) * clamp01(prob+sensitivityPP/100) * avgRate
		p.Rows = append(p.Rows, ProjectionRow{
			TypeName:    name,
			Active:      a.n,
			RenewalPct:  pct,
			MonthlyRate: round2(avgRate),
			Projected:   round2(projected),
		})
	}
	sort.Slice(p.Rows, func(i, j int) bool { return p.Rows[i].Projected > p.Rows[j].Projected })
	p.Total = round2(total)
	p.Low = round2(low)
	p.High = round2(high)
	return p
}

func productsDeepToWire(deep ProductsDeep, activeNow int) ProductsDeepWire {
	w := ProductsDeepWire{Rows: []ProductDeepWire{}}
	if activeNow > 0 {
		w.AttachRatePct = pctOf(deep.BuyersLast30, activeNow)
	}
	for _, r := range deep.Rows {
		row := ProductDeepWire{
			ProductID:  r.ProductID.String(),
			Name:       r.Name,
			Revenue30d: round2(r.Revenue30),
			Units30d:   r.Units30,
		}
		if r.AvgCost != nil && r.Revenue30 > 0 {
			margin := (r.Revenue30 - float64(r.Units30)**r.AvgCost) / r.Revenue30 * 100
			row.MarginPct = &margin
		}
		if r.Units30 > 0 {
			days := float64(r.Stock) / (float64(r.Units30) / 30.0)
			row.DaysOfStock = &days
		}
		w.Rows = append(w.Rows, row)
	}
	return w
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func pctOf(part, base int) *float64 {
	if base <= 0 {
		return nil
	}
	pct := float64(part) / float64(base) * 100
	return &pct
}

func round2(v float64) float64 {
	if v < 0 {
		return float64(int64(v*100-0.5)) / 100
	}
	return float64(int64(v*100+0.5)) / 100
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// monthsSince — meses calendario completos desde el mes de la cohorte
// ("2026-06") hasta hoy. Junio→agosto = 2.
func monthsSince(cohortMonth string, today time.Time) int {
	t, err := time.Parse("2006-01", cohortMonth)
	if err != nil {
		return 0
	}
	return (today.Year()-t.Year())*12 + int(today.Month()) - int(t.Month())
}
