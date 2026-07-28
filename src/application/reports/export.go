// UC-036 — Exportar reportes (PDF / XLSX).
//
// One use case orchestrates: load a report's underlying data via the Reader,
// then dispatch to the right renderer (gofpdf or excelize). The renderers
// stay in their own files (export_pdf.go, export_xlsx.go) so they can be
// stubbed in tests independently.
package reports

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// ReportType enumerates the exportable reports (DA-36.1).
const (
	ReportTypeMembers           = "members"
	ReportTypePayments          = "payments"
	ReportTypeSales             = "sales"
	ReportTypeCashClose         = "cash_close"
	ReportTypeAttentionRequired = "attention_required"
	// ReportTypePeriodSummary — UC-036 export del range report completo
	// (KPIs con deltas, ingresos vs egresos diarios, gastos por
	// categoría, top productos, stock crítico, gastos del período,
	// compras de inventario). Filename + header reflejan el rango real.
	ReportTypePeriodSummary = "period_summary"
)

// Format mirrors `format` query param.
const (
	FormatPDF  = "pdf"
	FormatXLSX = "xlsx"
)

// ErrUnsupportedReport / ErrUnsupportedFormat surface to the controller as
// 400s.
var (
	ErrUnsupportedReport = errors.New("tipo de reporte no soportado")
	ErrUnsupportedFormat = errors.New("formato no soportado (usa pdf o xlsx)")
)

type ExportInput struct {
	GymID  uuid.UUID
	Type   string
	Format string
	From   *time.Time // optional — date-bounded reports use it
	To     *time.Time
	// Period is consumed by ReportTypePeriodSummary so the export matches
	// the exact window the FE was rendering (incluyendo "custom").
	Period string
}

type ExportOutput struct {
	Filename    string
	ContentType string
	Bytes       []byte
}

type ExportReport struct {
	Reader            Reader
	Gyms              gymRepo.GymRepository
	UoW               sharedDomain.UnitOfWork
	AttentionRequired *AttentionRequired
	// Range — usado por ReportTypePeriodSummary para reusar la misma
	// agregación que alimenta la página de reportes (KPIs con deltas,
	// breakdowns, series diarias). Cuando es nil el export se rechaza
	// con ErrUnsupportedReport.
	Range *RangeReport
}

func NewExportReport(reader Reader, gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork,
	attention *AttentionRequired, rangeReport *RangeReport) *ExportReport {
	return &ExportReport{Reader: reader, Gyms: gyms, UoW: uow, AttentionRequired: attention, Range: rangeReport}
}

func (uc *ExportReport) Execute(ctx context.Context, in ExportInput) (*ExportOutput, error) {
	if in.Format != FormatPDF && in.Format != FormatXLSX {
		return nil, sharedDomain.NewValidationError(ErrUnsupportedFormat)
	}
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	gym, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return nil, err
	}
	from, to := defaultRange(gym.Timezone, in.From, in.To)

	switch in.Type {
	case ReportTypeMembers:
		rows, err := uc.Reader.ListMembersForExport(tx, in.GymID, time.Now().UTC())
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		return finalize(in, gym, "Listado de socios", from, to,
			func() []byte { return renderMembersPDF(gym, rows) },
			func() ([]byte, error) { return renderMembersXLSX(rows) })

	case ReportTypePayments:
		rows, err := uc.Reader.ListPaymentsForExport(tx, in.GymID, from, to)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		return finalize(in, gym, "Pagos", from, to,
			func() []byte { return renderPaymentsPDF(gym, rows, from, to) },
			func() ([]byte, error) { return renderPaymentsXLSX(rows) })

	case ReportTypeSales:
		rows, err := uc.Reader.ListSalesForExport(tx, in.GymID, gym.Timezone, from, to)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		return finalize(in, gym, "Ventas", from, to,
			func() []byte { return renderSalesPDF(gym, rows, from, to) },
			func() ([]byte, error) { return renderSalesXLSX(rows) })

	case ReportTypeAttentionRequired:
		out, err := uc.AttentionRequired.Execute(ctx, AttentionRequiredInput{GymID: in.GymID})
		if err != nil {
			return nil, err
		}
		return finalize(in, gym, "Atención inmediata", from, to,
			func() []byte { return renderAttentionRequiredPDF(gym, out) },
			func() ([]byte, error) { return renderAttentionRequiredXLSX(out) })

	case ReportTypeCashClose:
		// Cash close has its own dedicated endpoint but the export feature
		// also offers a per-day PDF/XLSX. We delegate to the same Reader
		// helpers (payments by date) — the operator picks the date via
		// from=to=today. The format stays simple.
		rows, err := uc.Reader.ListPaymentsForExport(tx, in.GymID, from, to)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		return finalize(in, gym, "Corte de caja", from, to,
			func() []byte { return renderPaymentsPDF(gym, rows, from, to) },
			func() ([]byte, error) { return renderPaymentsXLSX(rows) })

	case ReportTypePeriodSummary:
		if uc.Range == nil {
			return nil, sharedDomain.NewValidationError(ErrUnsupportedReport)
		}
		// Period summary: reuse the same use case the FE consumes so the
		// numbers/tables match exactly. When period == "custom" the
		// controller forwards in.From / in.To; otherwise PeriodWindow
		// computes the right range. We then re-derive from/to from the
		// output's strings so the filename + header reflect that window.
		period := in.Period
		if period == "" {
			period = PeriodMonth
		}
		rangeOut, err := uc.Range.Execute(ctx, RangeReportInput{
			GymID: in.GymID, Period: period, From: in.From, To: in.To,
		})
		if err != nil {
			return nil, err
		}
		realFrom, _ := time.Parse("2006-01-02", rangeOut.From)
		realTo, _ := time.Parse("2006-01-02", rangeOut.To)
		return finalize(in, gym, "Resumen del período", realFrom, realTo,
			func() []byte { return renderPeriodSummaryPDF(gym, rangeOut, realFrom, realTo) },
			func() ([]byte, error) { return renderPeriodSummaryXLSX(rangeOut, realFrom, realTo) })
	}
	return nil, sharedDomain.NewValidationError(ErrUnsupportedReport)
}

func finalize(in ExportInput, gym any, title string, from, to time.Time,
	pdf func() []byte, xlsx func() ([]byte, error)) (*ExportOutput, error) {
	_ = gym
	switch in.Format {
	case FormatPDF:
		out := pdf()
		return &ExportOutput{
			Filename:    filename(in.Type, "pdf", from, to),
			ContentType: "application/pdf",
			Bytes:       out,
		}, nil
	case FormatXLSX:
		out, err := xlsx()
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		return &ExportOutput{
			Filename:    filename(in.Type, "xlsx", from, to),
			ContentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			Bytes:       out,
		}, nil
	}
	_ = title
	return nil, sharedDomain.NewValidationError(ErrUnsupportedFormat)
}

func filename(reportType, ext string, from, to time.Time) string {
	safe := strings.ReplaceAll(reportType, "_", "-")
	return fmt.Sprintf("%s_%s_%s.%s", safe, from.Format("20060102"), to.Format("20060102"), ext)
}

// defaultRange yields the current month when neither bound is provided.
// Ambos extremos inclusivos y con granularidad de día.
//
// El "hoy" y la frontera de mes salen del día calendario del GYM, no del de
// UTC: el FE normalmente manda from/to explícitos, pero cuando no lo hace y
// el dueño exportaba a las 8 PM de CDMX, el default caía en el día — y el
// primer día de mes, en el MES — siguiente. Zona vacía = día UTC
// (fail-open, ver tz.LocalToday).
func defaultRange(tzName string, from, to *time.Time) (time.Time, time.Time) {
	today := tz.LocalToday(tzName, nowUTC())
	out := from
	end := to
	if out == nil {
		first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		out = &first
	}
	if end == nil {
		end = &today
	}
	return *out, *end
}
