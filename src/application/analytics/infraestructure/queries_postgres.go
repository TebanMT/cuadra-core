//go:build server

// Package infraestructure — implementación Postgres del analytics.Reader.
// SOLO cloud: la pestaña Análisis vive en el dashboard; el sidecar no
// registra el endpoint, así que NO hay espejo SQLite que mantener (decisión
// del plan Reports-improve fase 3 — endpoint aparte, no engordar /reports).
//
// Convenciones compartidas con reports/infraestructure:
//   - payment_date / expense_date / start_date / expiry_date son DATE en el
//     día local del gym → se comparan con strings "2006-01-02", sin tz.
//   - created_at / checkin_at son instantes (TIMESTAMPTZ) → ventanas de
//     instantes (tz.DayBounds para días calendario, o rolling windows).
//   - dinero NUMERIC(12,2) → float64 directo.
package infraestructure

import (
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/analytics"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

const dateFmt = "2006-01-02"

type PostgresReader struct{}

func NewPostgresReader() *PostgresReader { return &PostgresReader{} }

// CurrentMonthlyRates — snapshot canónico de hoy. A diferencia de la query
// histórica, exige socio activo y una slot vigente (active o la replaced que
// todavía cubre hoy durante una renovación anticipada).
func (r *PostgresReader) CurrentMonthlyRates(tx sharedDomain.Transaction, gymID uuid.UUID, onDate time.Time) ([]analytics.MemberMonthlyRate, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		MemberID    uuid.UUID
		TypeName    string
		MonthlyRate float64
	}
	var rows []row
	day := onDate.Format(dateFmt)
	if err := gormTx.Raw(`
		SELECT DISTINCT ON (ms.member_id)
		       ms.member_id,
		       COALESCE(ms.type_name_snapshot, 'Sin tipo') AS type_name,
		       CASE
		         WHEN ms.duration_months_snapshot IS NOT NULL AND ms.duration_months_snapshot > 0
		           THEN ms.price_snapshot / ms.duration_months_snapshot
		         WHEN ms.duration_days_snapshot > 0
		           THEN ms.price_snapshot * 30.0 / ms.duration_days_snapshot
		         ELSE 0
		       END AS monthly_rate
		FROM memberships ms
		JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL AND m.status = 'active'
		WHERE ms.gym_id = ? AND ms.deleted_at IS NULL AND ms.status IN ('active', 'replaced')
		  AND ms.start_date <= ? AND ms.expiry_date >= ?
		ORDER BY ms.member_id, ms.expiry_date DESC`, gymID, day, day).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.MemberMonthlyRate, len(rows))
	for i, x := range rows {
		out[i] = analytics.MemberMonthlyRate{MemberID: x.MemberID, TypeName: x.TypeName, MonthlyRate: x.MonthlyRate}
	}
	return out, nil
}

// ActiveMonthlyRates — cobertura histórica. DISTINCT ON toma la vigencia de
// expiry más lejano si hay traslape; no usa statuses actuales porque hacerlo
// reescribiría el MRR pasado al marcar un socio como perdido/inactivo.
func (r *PostgresReader) ActiveMonthlyRates(tx sharedDomain.Transaction, gymID uuid.UUID, onDate time.Time) ([]analytics.MemberMonthlyRate, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		MemberID    uuid.UUID
		TypeName    string
		MonthlyRate float64
	}
	var rows []row
	day := onDate.Format(dateFmt)
	if err := gormTx.Raw(`
		SELECT DISTINCT ON (ms.member_id)
		       ms.member_id,
		       COALESCE(ms.type_name_snapshot, 'Sin tipo') AS type_name,
		       CASE
		         WHEN ms.duration_months_snapshot IS NOT NULL AND ms.duration_months_snapshot > 0
		           THEN ms.price_snapshot / ms.duration_months_snapshot
		         WHEN ms.duration_days_snapshot > 0
		           THEN ms.price_snapshot * 30.0 / ms.duration_days_snapshot
		         ELSE 0
		       END AS monthly_rate
		FROM memberships ms
		JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL
		WHERE ms.gym_id = ? AND ms.deleted_at IS NULL
		  AND ms.start_date <= ? AND ms.expiry_date >= ?
		ORDER BY ms.member_id, ms.expiry_date DESC`,
		gymID, day, day).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.MemberMonthlyRate, len(rows))
	for i, x := range rows {
		out[i] = analytics.MemberMonthlyRate{MemberID: x.MemberID, TypeName: x.TypeName, MonthlyRate: x.MonthlyRate}
	}
	return out, nil
}

// CountNewMembersBetween — mismo criterio que reports: altas por created_at
// (instante) traducido a días locales vía tz.DayBounds.
func (r *PostgresReader) CountNewMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	start, end := tz.DayBounds(tzName, from, to)
	err := gormTx.Raw(`
		SELECT COUNT(*) FROM members
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND created_at >= ? AND created_at < ?`,
		gymID, start, end).Scan(&n).Error
	return int(n), err
}

// FirstMembershipCohorts — cohorte = mes de la PRIMERA membresía del socio.
// Retención a k meses: existe una membresía (cualquiera, no borrada) que
// cubre la fecha alta + k meses. pending_payment queda fuera solo porque su
// expiry_date NULL nunca pasa el >=.
func (r *PostgresReader) FirstMembershipCohorts(tx sharedDomain.Transaction, gymID uuid.UUID, monthsBack int, today time.Time) ([]analytics.CohortRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if monthsBack <= 0 {
		monthsBack = 6
	}
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	since := monthStart.AddDate(0, -(monthsBack - 1), 0)
	type row struct {
		CohortMonth string
		TypeName    string
		Size        int
		RetM1       int
		RetM2       int
		RetM3       int
	}
	var rows []row
	if err := gormTx.Raw(`
		WITH firsts AS (
			SELECT DISTINCT ON (ms.member_id)
			       ms.member_id, ms.start_date,
			       COALESCE(ms.type_name_snapshot, 'Sin tipo') AS type_name
			FROM memberships ms
			JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL
			WHERE ms.gym_id = ? AND ms.deleted_at IS NULL
			ORDER BY ms.member_id, ms.start_date ASC
		)
		SELECT to_char(date_trunc('month', f.start_date), 'YYYY-MM') AS cohort_month,
		       f.type_name,
		       COUNT(*) AS size,
		       COUNT(*) FILTER (WHERE EXISTS (
		         SELECT 1 FROM memberships r WHERE r.member_id = f.member_id AND r.deleted_at IS NULL
		           AND r.start_date <= (f.start_date + interval '1 month')::date
		           AND r.expiry_date >= (f.start_date + interval '1 month')::date)) AS ret_m1,
		       COUNT(*) FILTER (WHERE EXISTS (
		         SELECT 1 FROM memberships r WHERE r.member_id = f.member_id AND r.deleted_at IS NULL
		           AND r.start_date <= (f.start_date + interval '2 months')::date
		           AND r.expiry_date >= (f.start_date + interval '2 months')::date)) AS ret_m2,
		       COUNT(*) FILTER (WHERE EXISTS (
		         SELECT 1 FROM memberships r WHERE r.member_id = f.member_id AND r.deleted_at IS NULL
		           AND r.start_date <= (f.start_date + interval '3 months')::date
		           AND r.expiry_date >= (f.start_date + interval '3 months')::date)) AS ret_m3
		FROM firsts f
		WHERE f.start_date >= ?
		GROUP BY 1, 2
		ORDER BY 1 DESC, 3 DESC`,
		gymID, since.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.CohortRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.CohortRow{
			CohortMonth: x.CohortMonth, TypeName: x.TypeName,
			Size: x.Size, RetM1: x.RetM1, RetM2: x.RetM2, RetM3: x.RetM3,
		}
	}
	return out, nil
}

// AtRiskCandidates — señales crudas por socio activo: check-ins últimos 14
// días vs su propio promedio por-14d de las 8 semanas previas (ventanas de
// instantes rodantes — a esta granularidad la frontera del día local no
// mueve la señal), deuda viva total y vencimiento.
func (r *PostgresReader) AtRiskCandidates(tx sharedDomain.Transaction, gymID uuid.UUID, now, today time.Time) ([]analytics.AtRiskRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	last14 := now.AddDate(0, 0, -14)
	prev56 := now.AddDate(0, 0, -70)
	paidSince := today.AddDate(0, 0, -90)
	type row struct {
		MemberID    uuid.UUID
		FullName    string
		Phone       string
		Expiry      *time.Time
		Checkins14d int
		Avg14d      float64
		Balance     float64
		Paid90d     float64
	}
	var rows []row
	if err := gormTx.Raw(`
		WITH act AS (
			SELECT m.id, m.full_name, m.phone, MAX(ms.expiry_date) AS expiry
			FROM members m
			JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL
			WHERE m.gym_id = ? AND m.status = 'active' AND m.deleted_at IS NULL
			  AND ms.status IN ('active', 'replaced')
			  AND ms.start_date <= ? AND ms.expiry_date >= ?
			GROUP BY m.id
		),
		c14 AS (
			SELECT member_id, COUNT(*) AS n FROM checkins
			WHERE gym_id = ? AND deleted_at IS NULL AND result LIKE 'allowed%'
			  AND checkin_at >= ?
			GROUP BY member_id
		),
		cprev AS (
			SELECT member_id, COUNT(*)::float / 4 AS avg14 FROM checkins
			WHERE gym_id = ? AND deleted_at IS NULL AND result LIKE 'allowed%'
			  AND checkin_at >= ? AND checkin_at < ?
			GROUP BY member_id
		),
		debt AS (
			SELECT member_id, COALESCE(SUM(balance_pending), 0) AS bal FROM payments
			WHERE gym_id = ? AND deleted_at IS NULL AND member_id IS NOT NULL
			GROUP BY member_id
		),
		paid AS (
			SELECT member_id, COALESCE(SUM(amount), 0) AS total FROM payments
			WHERE gym_id = ? AND deleted_at IS NULL AND member_id IS NOT NULL
			  AND concept <> 'refund' AND payment_date >= ?
			GROUP BY member_id
		)
		SELECT act.id AS member_id, act.full_name, act.phone, act.expiry,
		       COALESCE(c14.n, 0) AS checkins14d,
		       COALESCE(cprev.avg14, 0) AS avg14d,
		       COALESCE(debt.bal, 0) AS balance,
		       COALESCE(paid.total, 0) AS paid90d
		FROM act
		LEFT JOIN c14 ON c14.member_id = act.id
		LEFT JOIN cprev ON cprev.member_id = act.id
		LEFT JOIN debt ON debt.member_id = act.id
		LEFT JOIN paid ON paid.member_id = act.id`,
		gymID, today.Format(dateFmt), today.Format(dateFmt),
		gymID, last14,
		gymID, prev56, last14,
		gymID,
		gymID, paidSince.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.AtRiskRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.AtRiskRow{
			MemberID: x.MemberID, FullName: x.FullName, Phone: x.Phone,
			ExpiryDate: x.Expiry, Checkins14d: x.Checkins14d, AvgPer14d: x.Avg14d,
			Balance: x.Balance, Paid90d: x.Paid90d,
		}
	}
	return out, nil
}

// MonthlyPL — modelo simple por mes: ingresos, compras completas de producto,
// gastos y devoluciones. COGS remains only as coverage/advanced product data.
func (r *PostgresReader) MonthlyPL(tx sharedDomain.Transaction, gymID uuid.UUID, monthsBack int, today time.Time) ([]analytics.PLMonthRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if monthsBack <= 0 {
		monthsBack = 6
	}
	var timezone string
	if err := gormTx.Raw(`SELECT COALESCE(NULLIF(timezone,''),'UTC') FROM gyms WHERE id=?`, gymID).Scan(&timezone).Error; err != nil {
		return nil, err
	}
	canonical := reportsInfra.NewPostgresReader()
	currentMonth := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := make([]analytics.PLMonthRow, 0, monthsBack)
	for offset := monthsBack - 1; offset >= 0; offset-- {
		from := currentMonth.AddDate(0, -offset, 0)
		to := from.AddDate(0, 1, -1)
		if to.After(today) {
			to = today
		}
		snapshot, err := canonical.CanonicalFinancialBetween(tx, gymID, timezone, from, to)
		if err != nil {
			return nil, err
		}
		profitRows, err := canonical.ProductProfitabilityBetween(tx, gymID, from, to)
		if err != nil {
			return nil, err
		}
		row := analytics.PLMonthRow{
			Month:            from.Format("2006-01"),
			Income:           snapshot.MembershipIncome + snapshot.ProductIncome + snapshot.OtherIncome + snapshot.UnclassifiedIncome,
			ProductPurchases: snapshot.InventoryPurchases,
			Expenses:         snapshot.OperatingExpenses, Refunds: snapshot.Refunds,
			COGSItemsTotal: len(profitRows),
		}
		for _, product := range profitRows {
			row.COGS += product.COGS
			if product.CostComplete {
				row.COGSItemsWithCost++
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// RetentionSince — base = socios (no borrados) con cobertura de fechas en
// `since`; retained = siguen cubiertos hoy. Split por bucket de género
// (NULL → no_especificado, igual que reports).
func (r *PostgresReader) RetentionSince(tx sharedDomain.Transaction, gymID uuid.UUID, since, today time.Time) ([]analytics.GenderRetentionRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		Bucket   string
		Base     int
		Retained int
	}
	var rows []row
	if err := gormTx.Raw(`
		WITH base AS (
			SELECT m.id, COALESCE(m.gender, 'no_especificado') AS bucket
			FROM members m
			WHERE m.gym_id = ? AND m.deleted_at IS NULL
			  AND EXISTS (SELECT 1 FROM memberships ms
			    WHERE ms.member_id = m.id AND ms.deleted_at IS NULL
			      AND ms.start_date <= ? AND ms.expiry_date >= ?)
		)
		SELECT b.bucket, COUNT(*) AS base,
		       COUNT(*) FILTER (WHERE EXISTS (SELECT 1 FROM memberships r
		         WHERE r.member_id = b.id AND r.deleted_at IS NULL
		           AND r.start_date <= ? AND r.expiry_date >= ?)) AS retained
		FROM base b
		GROUP BY b.bucket`,
		gymID, since.Format(dateFmt), since.Format(dateFmt),
		today.Format(dateFmt), today.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.GenderRetentionRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.GenderRetentionRow{Bucket: x.Bucket, Base: x.Base, Retained: x.Retained}
	}
	return out, nil
}

// RenewalRatesByType — vencimientos de los últimos 90 días
// por tipo; renovado = membresía nueva arrancando en [vencimiento−5,
// vencimiento+15] días.
func (r *PostgresReader) RenewalRatesByType(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]analytics.RenewalRateRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	since := today.AddDate(0, 0, -90)
	type row struct {
		TypeID      uuid.UUID
		TypeName    string
		Expirations int
		Renewed     int
	}
	var rows []row
	if err := gormTx.Raw(`
		WITH expired AS (
			SELECT ms.id, ms.member_id, ms.membership_type_id,
			       COALESCE(ms.type_name_snapshot, 'Sin tipo') AS type_name, ms.expiry_date
			FROM memberships ms
			JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL
			WHERE ms.gym_id = ? AND ms.deleted_at IS NULL
			  AND ms.expiry_date >= ? AND ms.expiry_date < ?
		)
		SELECT e.membership_type_id AS type_id, MAX(e.type_name) AS type_name,
		       COUNT(*) AS expirations,
		       COUNT(*) FILTER (WHERE EXISTS (SELECT 1 FROM memberships r
		         WHERE r.member_id = e.member_id AND r.deleted_at IS NULL
		           AND r.id <> e.id
		           AND r.start_date > e.expiry_date - 5
		           AND r.start_date <= e.expiry_date + 15)) AS renewed
		FROM expired e
		GROUP BY e.membership_type_id
		ORDER BY expirations DESC`,
		gymID, since.Format(dateFmt), today.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.RenewalRateRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.RenewalRateRow{TypeID: x.TypeID, TypeName: x.TypeName, Expirations: x.Expirations, Renewed: x.Renewed}
	}
	return out, nil
}

func (r *PostgresReader) RenewalsDueNextMonth(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]analytics.RenewalDueRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	nextMonth := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	afterNext := nextMonth.AddDate(0, 1, 0)
	type row struct {
		TypeID        uuid.UUID
		TypeName      string
		Due           int
		RenewalAmount float64
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT ms.membership_type_id AS type_id,
		       COALESCE(MAX(mt.name), MAX(ms.type_name_snapshot), 'Sin tipo') AS type_name,
		       COUNT(*) AS due,
		       AVG(COALESCE(mt.price, ms.price_snapshot)) AS renewal_amount
		FROM memberships ms
		JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL AND m.status = 'active'
		LEFT JOIN membership_types mt ON mt.id = ms.membership_type_id AND mt.deleted_at IS NULL
		WHERE ms.gym_id = ? AND ms.deleted_at IS NULL
		  AND ms.status = 'active'
		  AND ms.expiry_date >= ? AND ms.expiry_date < ?
		GROUP BY ms.membership_type_id
		ORDER BY due DESC, type_name`, gymID, nextMonth.Format(dateFmt), afterNext.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.RenewalDueRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.RenewalDueRow(x)
	}
	return out, nil
}

// ProductsDeep — revenue y COGS cash-based 30d por producto. Un pago se
// reparte por el peso de la línea en la venta; abonos y refunds siguen al
// pago padre. Las unidades siguen siendo físicas (ventas originadas 30d).
func (r *PostgresReader) ProductsDeep(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (analytics.ProductsDeep, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	since := today.AddDate(0, 0, -29).Format(dateFmt)
	day := today.Format(dateFmt)
	var out analytics.ProductsDeep

	var buyers int64
	if err := gormTx.Raw(`
		SELECT COUNT(DISTINCT p.member_id) FROM payments p
		WHERE p.gym_id = ? AND p.deleted_at IS NULL
		  AND p.member_id IS NOT NULL AND p.payment_date >= ? AND p.payment_date <= ?
		  AND (p.concept = 'product' OR (p.concept = 'balance_settlement' AND EXISTS (
		    SELECT 1 FROM sales s WHERE s.payment_id = p.parent_payment_id AND s.deleted_at IS NULL
		  )))`,
		gymID, since, day).Scan(&buyers).Error; err != nil {
		return out, err
	}
	out.BuyersLast30 = int(buyers)

	profitRows, err := reportsInfra.NewPostgresReader().ProductProfitabilityBetween(
		tx, gymID, today.AddDate(0, 0, -29), today,
	)
	if err != nil {
		return out, err
	}
	type stockRow struct {
		ID    uuid.UUID
		Stock int
	}
	var stocks []stockRow
	if err := gormTx.Raw(`SELECT id,stock FROM products WHERE gym_id=?`, gymID).Scan(&stocks).Error; err != nil {
		return out, err
	}
	stockByID := make(map[uuid.UUID]int, len(stocks))
	for _, stock := range stocks {
		stockByID[stock.ID] = stock.Stock
	}
	if len(profitRows) > 10 {
		profitRows = profitRows[:10]
	}
	out.Rows = make([]analytics.ProductDeepRow, len(profitRows))
	for i, product := range profitRows {
		var costMarker *float64
		if product.CostComplete {
			value := 0.0
			if product.Quantity != 0 {
				value = math.Abs(product.COGS / float64(product.Quantity))
			}
			costMarker = &value
		}
		out.Rows[i] = analytics.ProductDeepRow{
			ProductID: product.ProductID, Name: product.ProductName, Stock: stockByID[product.ProductID],
			Units30: product.Quantity, Revenue30: product.Revenue, COGS30: product.COGS, AvgCost: costMarker,
		}
	}
	return out, nil
}

// GenderActivity — por bucket de género: activos hoy, check-ins 30d
// (instantes, ventana rodante) y gasto 30d (payment_date). La retención
// por bucket viene de RetentionSince; el use case las cruza.
func (r *PostgresReader) GenderActivity(tx sharedDomain.Transaction, gymID uuid.UUID, now, today time.Time) ([]analytics.GenderActivityRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	day := today.Format(dateFmt)
	spendSince := today.AddDate(0, 0, -29).Format(dateFmt)
	checkinsSince := now.AddDate(0, 0, -30)
	type row struct {
		Bucket      string
		Active      int
		Checkins30d int
		Spend30d    float64
	}
	var rows []row
	if err := gormTx.Raw(`
		WITH act AS (
			SELECT m.id, COALESCE(m.gender, 'no_especificado') AS bucket
			FROM members m
			JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL
			WHERE m.gym_id = ? AND m.status = 'active' AND m.deleted_at IS NULL
			  AND ms.status IN ('active', 'replaced')
			  AND ms.start_date <= ? AND ms.expiry_date >= ?
			GROUP BY m.id
		),
		c AS (
			SELECT member_id, COUNT(*) AS n FROM checkins
			WHERE gym_id = ? AND deleted_at IS NULL AND result LIKE 'allowed%'
			  AND checkin_at >= ?
			GROUP BY member_id
		),
		p AS (
			SELECT member_id, SUM(amount) AS total FROM payments
			WHERE gym_id = ? AND deleted_at IS NULL AND member_id IS NOT NULL
			  AND concept <> 'refund' AND payment_date >= ?
			GROUP BY member_id
		)
		SELECT act.bucket,
		       COUNT(*) AS active,
		       COALESCE(SUM(c.n), 0) AS checkins30d,
		       COALESCE(SUM(p.total), 0) AS spend30d
		FROM act
		LEFT JOIN c ON c.member_id = act.id
		LEFT JOIN p ON p.member_id = act.id
		GROUP BY act.bucket`,
		gymID, day, day,
		gymID, checkinsSince,
		gymID, spendSince).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.GenderActivityRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.GenderActivityRow{
			Bucket: x.Bucket, Active: x.Active,
			Checkins30d: x.Checkins30d, Spend30d: x.Spend30d,
		}
	}
	return out, nil
}

// agePyramidBuckets — orden fijo del wire (el FE pinta la pirámide en este
// orden sin reordenar).
var agePyramidBuckets = []string{"<18", "18-24", "25-34", "35-44", "45-54", "55+"}

// AgePyramid — activos por bucket de edad × género. Sin birthdate cuentan
// aparte (noBirthdate) para que la pirámide sea honesta con su cobertura.
func (r *PostgresReader) AgePyramid(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]analytics.AgePyramidRow, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	day := today.Format(dateFmt)
	type row struct {
		Bucket         string
		Hombre         int
		Mujer          int
		NoEspecificado int
	}
	var rows []row
	if err := gormTx.Raw(`
		WITH act AS (
			SELECT m.id, m.birthdate, COALESCE(m.gender, 'no_especificado') AS g
			FROM members m
			JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL
			WHERE m.gym_id = ? AND m.status = 'active' AND m.deleted_at IS NULL
			  AND ms.status IN ('active', 'replaced')
			  AND ms.start_date <= ? AND ms.expiry_date >= ?
			GROUP BY m.id
		),
		aged AS (
			SELECT g, CASE
			  WHEN birthdate IS NULL THEN NULL
			  ELSE date_part('year', age(?::date, birthdate))::int END AS years
			FROM act
		)
		SELECT COALESCE(CASE
		         WHEN years IS NULL THEN NULL
		         WHEN years < 18 THEN '<18'
		         WHEN years < 25 THEN '18-24'
		         WHEN years < 35 THEN '25-34'
		         WHEN years < 45 THEN '35-44'
		         WHEN years < 55 THEN '45-54'
		         ELSE '55+' END, 'sin_fecha') AS bucket,
		       COUNT(*) FILTER (WHERE g = 'hombre') AS hombre,
		       COUNT(*) FILTER (WHERE g = 'mujer') AS mujer,
		       COUNT(*) FILTER (WHERE g = 'no_especificado') AS no_especificado
		FROM aged
		GROUP BY 1`,
		gymID, day, day, day).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	byBucket := map[string]row{}
	noBirthdate := 0
	for _, x := range rows {
		if x.Bucket == "sin_fecha" {
			noBirthdate = x.Hombre + x.Mujer + x.NoEspecificado
			continue
		}
		byBucket[x.Bucket] = x
	}
	out := make([]analytics.AgePyramidRow, 0, len(agePyramidBuckets))
	for _, b := range agePyramidBuckets {
		x := byBucket[b]
		out = append(out, analytics.AgePyramidRow{
			Bucket: b, Hombre: x.Hombre, Mujer: x.Mujer, NoEspecificado: x.NoEspecificado,
		})
	}
	return out, noBirthdate, nil
}

// PaydayPattern — ingresos no-refund acumulados por día del mes en la
// ventana. El use case rellena los 31 días.
func (r *PostgresReader) PaydayPattern(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]analytics.PaydayRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		Day   int
		Total float64
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT EXTRACT(DAY FROM payment_date)::int AS day,
		       COALESCE(SUM(amount), 0) AS total
		FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL AND concept <> 'refund'
		  AND payment_date >= ? AND payment_date <= ?
		GROUP BY 1
		ORDER BY 1`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.PaydayRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.PaydayRow{Day: x.Day, Total: x.Total}
	}
	return out, nil
}

// FixedMonthlyCosts uses committed active fixed templates, normalized to a
// monthly equivalent. Variable marketing/maintenance never enter break-even.
func (r *PostgresReader) FixedMonthlyCosts(tx sharedDomain.Transaction, gymID uuid.UUID, monthsBack int, today time.Time) (float64, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if monthsBack <= 0 {
		monthsBack = 3
	}
	var out struct {
		Avg    float64
		Months int
	}
	err := gormTx.Raw(`
		SELECT COALESCE(SUM(expected_amount * CASE frequency
		 WHEN 'weekly' THEN 52.0/12 WHEN 'every_14_days' THEN 26.0/12
		 WHEN 'semimonthly' THEN 2 WHEN 'monthly' THEN 1 WHEN 'bimonthly' THEN .5
		 WHEN 'quarterly' THEN 1.0/3 WHEN 'semiannual' THEN 1.0/6 WHEN 'annual' THEN 1.0/12 ELSE 0 END),0) AS avg,
		 (SELECT COUNT(DISTINCT date_trunc('month',expense_date)) FROM expenses
		  WHERE gym_id=? AND deleted_at IS NULL
		    AND expense_date>=date_trunc('month',?::date)-(? * interval '1 month')) AS months
		FROM recurring_expense_templates
		WHERE gym_id=? AND deleted_at IS NULL AND active=true AND classification='fixed'`, gymID, today.Format(dateFmt), monthsBack, gymID).Scan(&out).Error
	return out.Avg, out.Months, err
}

// WeeklyHeatmap — check-ins exitosos de 8 semanas por ISODOW × hora LOCAL
// (sin la zona, el pico de las 7 PM de CDMX caía a la 1 AM del día
// siguiente — misma lección que el heatmap de género de reports).
func (r *PostgresReader) WeeklyHeatmap(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, now time.Time) ([]analytics.HeatCellRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	zone := tz.NameOrUTC(tzName)
	since := now.AddDate(0, 0, -56)
	type row struct {
		Dow   int
		Hour  int
		Count int
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT EXTRACT(ISODOW FROM (c.checkin_at AT TIME ZONE ?))::int AS dow,
		       EXTRACT(HOUR FROM (c.checkin_at AT TIME ZONE ?))::int AS hour,
		       COUNT(*) AS count
		FROM checkins c
		WHERE c.gym_id = ? AND c.deleted_at IS NULL AND c.result LIKE 'allowed%'
		  AND c.checkin_at >= ?
		GROUP BY 1, 2
		ORDER BY 1, 2`,
		zone, zone, gymID, since).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.HeatCellRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.HeatCellRow{Dow: x.Dow, Hour: x.Hour, Count: x.Count}
	}
	return out, nil
}

// CountReactivations — membresías arrancando en la ventana cuyo socio
// venía de un HUECO real: existe una previa vencida >15 días antes del
// arranque y NO había cobertura el día anterior (una renovación normal no
// cuenta).
func (r *PostgresReader) CountReactivations(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Raw(`
		SELECT COUNT(DISTINCT ms.member_id)
		FROM memberships ms
		JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL
		WHERE ms.gym_id = ? AND ms.deleted_at IS NULL
		  AND ms.start_date >= ? AND ms.start_date <= ?
		  AND EXISTS (SELECT 1 FROM memberships p
		    WHERE p.member_id = ms.member_id AND p.deleted_at IS NULL
		      AND p.expiry_date IS NOT NULL AND p.expiry_date < ms.start_date - 15)
		  AND NOT EXISTS (SELECT 1 FROM memberships c
		    WHERE c.member_id = ms.member_id AND c.deleted_at IS NULL AND c.id <> ms.id
		      AND c.start_date <= ms.start_date - 1 AND c.expiry_date >= ms.start_date - 1)`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&n).Error
	return int(n), err
}

// TenureMonths — mediana de vida (primer inicio → último vencimiento) de
// socios cuya cobertura terminó en los últimos 12 meses. En meses (~30.44
// días); nil cuando no hay churners.
func (r *PostgresReader) TenureMonths(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (*float64, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	day := today.Format(dateFmt)
	yearAgo := today.AddDate(-1, 0, 0).Format(dateFmt)
	var out struct {
		MedDays *float64
		N       int
	}
	err := gormTx.Raw(`
		WITH life AS (
			SELECT m.id, MIN(ms.start_date) AS s, MAX(ms.expiry_date) AS e
			FROM members m
			JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL
			  AND ms.expiry_date IS NOT NULL
			WHERE m.gym_id = ? AND m.deleted_at IS NULL
			GROUP BY m.id
			HAVING MAX(ms.expiry_date) < ? AND MAX(ms.expiry_date) >= ?
		)
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY (e - s)) AS med_days,
		       COUNT(*) AS n
		FROM life`,
		gymID, day, yearAgo).Scan(&out).Error
	if err != nil || out.MedDays == nil {
		return nil, out.N, err
	}
	months := *out.MedDays / 30.44
	return &months, out.N, nil
}

// PromotionsROI — por promoción: usos/descuento de 90d + base de retención
// (miembros que la usaron hace 60–180 días, madurez mínima de 60d) y cuántos
// siguen cubiertos hoy. El baseline es la misma ventana con pagos de
// membresía SIN promo aplicada.
func (r *PostgresReader) PromotionsROI(tx sharedDomain.Transaction, gymID uuid.UUID, now, today time.Time) (analytics.PromotionsROI, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var out analytics.PromotionsROI
	uses90 := now.AddDate(0, 0, -90)
	matureFrom := now.AddDate(0, 0, -180)
	matureTo := now.AddDate(0, 0, -60)
	day := today.Format(dateFmt)

	type row struct {
		PromotionID   uuid.UUID
		Name          string
		Uses90d       int
		Discount90d   float64
		PromoBase     int
		PromoRetained int
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT ap.promotion_id,
		       MIN(ap.promotion_name_snapshot) AS name,
		       COUNT(*) FILTER (WHERE ap.created_at >= ?) AS uses90d,
		       COALESCE(SUM(ap.discount_amount) FILTER (WHERE ap.created_at >= ?), 0) AS discount90d,
		       COUNT(DISTINCT ap.member_id) FILTER (
		         WHERE ap.member_id IS NOT NULL AND ap.created_at >= ? AND ap.created_at < ?) AS promo_base,
		       COUNT(DISTINCT ap.member_id) FILTER (
		         WHERE ap.member_id IS NOT NULL AND ap.created_at >= ? AND ap.created_at < ?
		           AND EXISTS (SELECT 1 FROM memberships r
		             WHERE r.member_id = ap.member_id AND r.deleted_at IS NULL
		               AND r.start_date <= ? AND r.expiry_date >= ?)) AS promo_retained
		FROM applied_promotions ap
		WHERE ap.gym_id = ? AND ap.deleted_at IS NULL
		GROUP BY ap.promotion_id
		HAVING COUNT(*) FILTER (WHERE ap.created_at >= ?) > 0
		    OR COUNT(*) FILTER (WHERE ap.created_at >= ? AND ap.created_at < ?) > 0
		ORDER BY uses90d DESC`,
		uses90, uses90,
		matureFrom, matureTo,
		matureFrom, matureTo, day, day,
		gymID,
		uses90, matureFrom, matureTo).Scan(&rows).Error; err != nil {
		return out, err
	}
	out.Rows = make([]analytics.PromotionROIRow, len(rows))
	for i, x := range rows {
		out.Rows[i] = analytics.PromotionROIRow{
			PromotionID: x.PromotionID, Name: x.Name,
			Uses90d: x.Uses90d, Discount90d: x.Discount90d,
			PromoBase: x.PromoBase, PromoRetained: x.PromoRetained,
		}
	}

	// Baseline precio completo: pagos de membresía en la misma ventana de
	// madurez SIN promo aplicada en ese pago.
	fullFrom := today.AddDate(0, 0, -180).Format(dateFmt)
	fullTo := today.AddDate(0, 0, -60).Format(dateFmt)
	var fp struct {
		Base     int
		Retained int
	}
	if err := gormTx.Raw(`
		WITH fp AS (
			SELECT DISTINCT p.member_id
			FROM payments p
			WHERE p.gym_id = ? AND p.deleted_at IS NULL AND p.concept = 'membership'
			  AND p.member_id IS NOT NULL
			  AND p.payment_date >= ? AND p.payment_date < ?
			  AND NOT EXISTS (SELECT 1 FROM applied_promotions ap
			    WHERE ap.payment_id = p.id AND ap.deleted_at IS NULL)
		)
		SELECT COUNT(*) AS base,
		       COUNT(*) FILTER (WHERE EXISTS (SELECT 1 FROM memberships r
		         WHERE r.member_id = fp.member_id AND r.deleted_at IS NULL
		           AND r.start_date <= ? AND r.expiry_date >= ?)) AS retained
		FROM fp`,
		gymID, fullFrom, fullTo, day, day).Scan(&fp).Error; err != nil {
		return out, err
	}
	out.FullPriceBase = fp.Base
	out.FullPriceRetained = fp.Retained
	return out, nil
}
