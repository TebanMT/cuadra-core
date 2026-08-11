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
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/analytics"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

const dateFmt = "2006-01-02"

type PostgresReader struct{}

func NewPostgresReader() *PostgresReader { return &PostgresReader{} }

// ActiveMonthlyRates — un socio activo = status='active' + membresía (no
// borrada) cuyo rango de fechas cubre onDate. DISTINCT ON toma la de expiry
// más lejano si hay traslape. La tarifa mensual-equivalente divide el precio
// del snapshot entre sus meses (fallback días/30).
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
		JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL AND m.status = 'active'
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

// MonthlyPL — P&L por mes calendario (SPEC §9.6): ingresos brutos, COGS al
// costo promedio all-time (mismo criterio que RealizedProductProfit de
// Standard, ventas no reembolsadas), gastos generales y devoluciones.
func (r *PostgresReader) MonthlyPL(tx sharedDomain.Transaction, gymID uuid.UUID, monthsBack int, today time.Time) ([]analytics.PLMonthRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if monthsBack <= 0 {
		monthsBack = 6
	}
	day := today.Format(dateFmt)
	type row struct {
		Month    string
		Income   float64
		Refunds  float64
		Expenses float64
		COGS     float64
	}
	var rows []row
	if err := gormTx.Raw(`
		WITH ac AS (
			SELECT product_id, SUM(cost * delta)::numeric / NULLIF(SUM(delta), 0) AS avg_cost
			FROM stock_movements
			WHERE gym_id = ? AND deleted_at IS NULL AND movement_type = 'restock'
			  AND cost IS NOT NULL AND delta > 0
			GROUP BY product_id
		),
		months AS (
			SELECT generate_series(
				date_trunc('month', ?::date) - (? - 1) * interval '1 month',
				date_trunc('month', ?::date),
				interval '1 month')::date AS mstart
		)
		SELECT to_char(m.mstart, 'YYYY-MM') AS month,
		  COALESCE((SELECT SUM(p.amount) FROM payments p
		    WHERE p.gym_id = ? AND p.deleted_at IS NULL AND p.concept <> 'refund'
		      AND p.payment_date >= m.mstart AND p.payment_date < (m.mstart + interval '1 month')::date), 0) AS income,
		  COALESCE((SELECT SUM(ABS(p.amount)) FROM payments p
		    WHERE p.gym_id = ? AND p.deleted_at IS NULL AND p.concept = 'refund'
		      AND p.payment_date >= m.mstart AND p.payment_date < (m.mstart + interval '1 month')::date), 0) AS refunds,
		  COALESCE((SELECT SUM(e.amount) FROM expenses e
		    WHERE e.gym_id = ? AND e.deleted_at IS NULL
		      AND e.expense_date >= m.mstart AND e.expense_date < (m.mstart + interval '1 month')::date), 0) AS expenses,
		  COALESCE((SELECT SUM(si.quantity * ac.avg_cost)
		    FROM sale_items si
		    JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
		    JOIN payments p ON p.id = s.payment_id AND p.deleted_at IS NULL
		    JOIN ac ON ac.product_id = si.product_id
		    WHERE si.gym_id = ? AND si.deleted_at IS NULL
		      AND p.payment_date >= m.mstart AND p.payment_date < (m.mstart + interval '1 month')::date
		      AND NOT EXISTS (SELECT 1 FROM payments rfd
		        WHERE rfd.parent_payment_id = p.id AND rfd.concept = 'refund' AND rfd.deleted_at IS NULL)), 0) AS cogs
		FROM months m
		ORDER BY m.mstart`,
		gymID, day, monthsBack, day,
		gymID, gymID, gymID, gymID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.PLMonthRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.PLMonthRow{
			Month: x.Month, Income: x.Income, COGS: x.COGS,
			Expenses: x.Expenses, Refunds: x.Refunds,
		}
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

// RenewalRatesByType — vencimientos (no reemplazados) de los últimos 90 días
// por tipo; renovado = membresía nueva arrancando en [vencimiento−5,
// vencimiento+15] días.
func (r *PostgresReader) RenewalRatesByType(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]analytics.RenewalRateRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	since := today.AddDate(0, 0, -90)
	type row struct {
		TypeName    string
		Expirations int
		Renewed     int
	}
	var rows []row
	if err := gormTx.Raw(`
		WITH expired AS (
			SELECT ms.member_id, COALESCE(ms.type_name_snapshot, 'Sin tipo') AS type_name, ms.expiry_date
			FROM memberships ms
			JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL
			WHERE ms.gym_id = ? AND ms.deleted_at IS NULL AND ms.replaced_by IS NULL
			  AND ms.expiry_date >= ? AND ms.expiry_date < ?
		)
		SELECT e.type_name, COUNT(*) AS expirations,
		       COUNT(*) FILTER (WHERE EXISTS (SELECT 1 FROM memberships r
		         WHERE r.member_id = e.member_id AND r.deleted_at IS NULL
		           AND r.start_date > e.expiry_date - 5
		           AND r.start_date <= e.expiry_date + 15)) AS renewed
		FROM expired e
		GROUP BY e.type_name
		ORDER BY expirations DESC`,
		gymID, since.Format(dateFmt), today.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]analytics.RenewalRateRow, len(rows))
	for i, x := range rows {
		out[i] = analytics.RenewalRateRow{TypeName: x.TypeName, Expirations: x.Expirations, Renewed: x.Renewed}
	}
	return out, nil
}

// ProductsDeep — top 10 productos activos por revenue 30d + compradores
// distintos (para el attach rate).
func (r *PostgresReader) ProductsDeep(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (analytics.ProductsDeep, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	since := today.AddDate(0, 0, -30).Format(dateFmt)
	var out analytics.ProductsDeep

	var buyers int64
	if err := gormTx.Raw(`
		SELECT COUNT(DISTINCT p.member_id) FROM payments p
		WHERE p.gym_id = ? AND p.deleted_at IS NULL AND p.concept = 'product'
		  AND p.member_id IS NOT NULL AND p.payment_date >= ?`,
		gymID, since).Scan(&buyers).Error; err != nil {
		return out, err
	}
	out.BuyersLast30 = int(buyers)

	type row struct {
		ProductID uuid.UUID
		Name      string
		Stock     int
		Units30   int
		Revenue30 float64
		AvgCost   *float64
	}
	var rows []row
	if err := gormTx.Raw(`
		WITH ac AS (
			SELECT product_id, SUM(cost * delta)::numeric / NULLIF(SUM(delta), 0) AS avg_cost
			FROM stock_movements
			WHERE gym_id = ? AND deleted_at IS NULL AND movement_type = 'restock'
			  AND cost IS NOT NULL AND delta > 0
			GROUP BY product_id
		),
		sold AS (
			SELECT si.product_id, SUM(si.quantity) AS units, SUM(si.line_total) AS revenue
			FROM sale_items si
			JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
			JOIN payments p ON p.id = s.payment_id AND p.deleted_at IS NULL AND p.concept <> 'refund'
			WHERE si.gym_id = ? AND si.deleted_at IS NULL AND p.payment_date >= ?
			GROUP BY si.product_id
		)
		SELECT pr.id AS product_id, pr.name, pr.stock,
		       COALESCE(sold.units, 0) AS units30,
		       COALESCE(sold.revenue, 0) AS revenue30,
		       ac.avg_cost
		FROM products pr
		LEFT JOIN sold ON sold.product_id = pr.id
		LEFT JOIN ac ON ac.product_id = pr.id
		WHERE pr.gym_id = ? AND pr.deleted_at IS NULL AND pr.active = TRUE
		ORDER BY COALESCE(sold.revenue, 0) DESC
		LIMIT 10`,
		gymID, gymID, since, gymID).Scan(&rows).Error; err != nil {
		return out, err
	}
	out.Rows = make([]analytics.ProductDeepRow, len(rows))
	for i, x := range rows {
		out.Rows[i] = analytics.ProductDeepRow{
			ProductID: x.ProductID, Name: x.Name, Stock: x.Stock,
			Units30: x.Units30, Revenue30: x.Revenue30, AvgCost: x.AvgCost,
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

// FixedMonthlyCosts — promedio mensual de gastos generales de los últimos
// N meses COMPLETOS, excluyendo mercadería (eso es costo de producto). El
// promedio es sobre los meses CON captura, no entre N — un gym que apenas
// empieza a capturar no diluye su único mes real.
func (r *PostgresReader) FixedMonthlyCosts(tx sharedDomain.Transaction, gymID uuid.UUID, monthsBack int, today time.Time) (float64, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if monthsBack <= 0 {
		monthsBack = 3
	}
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	from := monthStart.AddDate(0, -monthsBack, 0)
	var out struct {
		Avg    float64
		Months int
	}
	err := gormTx.Raw(`
		WITH m AS (
			SELECT date_trunc('month', e.expense_date)::date AS mstart,
			       SUM(e.amount) AS total
			FROM expenses e
			WHERE e.gym_id = ? AND e.deleted_at IS NULL
			  AND e.category <> 'mercaderia_externa'
			  AND e.expense_date >= ? AND e.expense_date < ?
			GROUP BY 1
		)
		SELECT COALESCE(AVG(total), 0) AS avg, COUNT(*) AS months FROM m`,
		gymID, from.Format(dateFmt), monthStart.Format(dateFmt)).Scan(&out).Error
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
