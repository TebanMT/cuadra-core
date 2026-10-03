package reports_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"github.com/cuadra/cuadra-core/src/application/reports"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// fakeGymRepo returns a static gym for the export use case. We don't exercise
// any other GymRepository methods; the broader interface stays satisfied via
// embedded composition.
type fakeGymRepo struct {
	gymRepo.GymRepository
	gym *gymDomain.Gym
}

func (f *fakeGymRepo) GetByID(_ sharedDomain.Transaction, _ uuid.UUID) (*gymDomain.Gym, error) {
	return f.gym, nil
}

func sampleGym() *gymDomain.Gym {
	name := "Gym Test"
	city := "CDMX"
	wa := "+5215555550000"
	return &gymDomain.Gym{
		ID:                 uuid.New(),
		Name:               &name,
		City:               &city,
		WhatsApp:           &wa,
		Country:            "MX",
		Timezone:           "America/Mexico_City",
		SubscriptionPlan:   "trial",
		SubscriptionStatus: "active",
	}
}

func newExportFixture(t *testing.T, reader *fakeReader) *reports.ExportReport {
	t.Helper()
	att := reports.NewAttentionRequired(reader, fakeUoW{})
	rng := reports.NewRangeReport(reader, fakeUoW{})
	gyms := &fakeGymRepo{gym: sampleGym()}
	return reports.NewExportReport(reader, gyms, fakeUoW{}, att, rng)
}

func TestExportPDF_MembersHasPDFHeader(t *testing.T) {
	reader := &fakeReader{
		exportMembers: []reports.MemberExportRow{
			{Folio: "MEM-000001", FullName: "Juan Pérez", Phone: "5555550001",
				Status: "active", PlanName: ptr("Mensual"), CreatedAt: time.Now().UTC()},
		},
	}
	uc := newExportFixture(t, reader)
	out, err := uc.Execute(context.Background(), reports.ExportInput{
		GymID: uuid.New(), Type: reports.ReportTypeMembers, Format: reports.FormatPDF,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !bytes.HasPrefix(out.Bytes, []byte("%PDF-")) {
		t.Errorf("output is not a PDF: first bytes = %q", out.Bytes[:8])
	}
	if !strings.HasSuffix(out.Filename, ".pdf") {
		t.Errorf("filename %q does not end in .pdf", out.Filename)
	}
	if out.ContentType != "application/pdf" {
		t.Errorf("content-type = %q", out.ContentType)
	}
}

func TestExportXLSX_PaymentsRoundtrips(t *testing.T) {
	reader := &fakeReader{
		exportPayments: []reports.PaymentExportRow{
			{Folio: "PAGO/0001", PaymentDate: time.Now().UTC(),
				MemberFullName: ptr("María"), Concept: "membership", Method: "cash",
				Amount: 500, Discount: 0, BalancePending: 0},
			{Folio: "PAGO/0002", PaymentDate: time.Now().UTC(),
				Concept: "product", Method: "card", Amount: 75},
		},
	}
	uc := newExportFixture(t, reader)
	out, err := uc.Execute(context.Background(), reports.ExportInput{
		GymID: uuid.New(), Type: reports.ReportTypePayments, Format: reports.FormatXLSX,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.ContentType != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Errorf("content-type = %q", out.ContentType)
	}
	f, err := excelize.OpenReader(bytes.NewReader(out.Bytes))
	if err != nil {
		t.Fatalf("xlsx parse: %v", err)
	}
	defer f.Close()
	rows, err := f.GetRows("Sheet1")
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}
	if len(rows) != 3 { // header + 2 rows
		t.Errorf("expected 3 rows (1 header + 2 data), got %d", len(rows))
	}
	if rows[0][0] != "Folio" {
		t.Errorf("header[0] = %q, want Folio", rows[0][0])
	}
	if rows[1][0] != "PAGO/0001" {
		t.Errorf("row1.folio = %q", rows[1][0])
	}
}

func TestExport_RejectsBadFormat(t *testing.T) {
	uc := newExportFixture(t, &fakeReader{})
	_, err := uc.Execute(context.Background(), reports.ExportInput{
		GymID: uuid.New(), Type: reports.ReportTypeMembers, Format: "csv",
	})
	if err == nil {
		t.Fatal("expected error for bad format")
	}
}

func TestExport_RejectsBadType(t *testing.T) {
	uc := newExportFixture(t, &fakeReader{})
	_, err := uc.Execute(context.Background(), reports.ExportInput{
		GymID: uuid.New(), Type: "bogus", Format: reports.FormatPDF,
	})
	if err == nil {
		t.Fatal("expected error for bad type")
	}
}

func TestExportXLSX_AttentionRequiredHasSections(t *testing.T) {
	reader := &fakeReader{
		expiringSoon: []reports.MemberExpiringRow{
			{MemberID: uuid.New(), FullName: "X", Phone: "1", ExpiryDate: time.Now().UTC(), DaysLeft: 3},
		},
		lowStock: []reports.ProductLowStockRow{
			{ProductID: uuid.New(), Name: "Agua", Stock: 1, StockMinimum: 5},
		},
	}
	uc := newExportFixture(t, reader)
	out, err := uc.Execute(context.Background(), reports.ExportInput{
		GymID: uuid.New(), Type: reports.ReportTypeAttentionRequired, Format: reports.FormatXLSX,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(out.Bytes))
	if err != nil {
		t.Fatalf("xlsx parse: %v", err)
	}
	defer f.Close()
	rows, err := f.GetRows("Sheet1")
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	// At least one section header (Vencen próximamente) and the entries
	// should be present.
	found := false
	for _, r := range rows {
		if len(r) > 0 && strings.Contains(r[0], "Vencen") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected section header in output, got %v", rows)
	}
}

func ptr(s string) *string { return &s }

// TestExport_PeriodSummary_PDF verifies the period_summary export emits a
// valid PDF with a sensible filename. We don't substring-search the bytes
// because gofpdf compresses content streams — the XLSX test below covers
// section-by-section assertions, which is what we'd be checking anyway.
func TestExport_PeriodSummary_PDF(t *testing.T) {
	reader := &fakeReader{
		incomeNow:       1500,
		generalExpenses: 200,
		inventoryCost:   100,
		newMembersCount: 1,
		checkinsCount:   5,
		expensesByCategory: map[string]float64{
			"renta":     1200,
			"servicios": 300,
		},
		topProducts: []reports.TopProductRow{
			{ProductID: uuid.New(), ProductName: "Agua 600ml", Quantity: 15, Revenue: 225},
		},
		criticalStock: reports.CriticalStockCounts{OutCount: 2, LowCount: 3},
	}
	uc := newExportFixture(t, reader)
	out, err := uc.Execute(context.Background(), reports.ExportInput{
		GymID:  uuid.New(),
		Type:   reports.ReportTypePeriodSummary,
		Format: reports.FormatPDF,
		Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !bytes.HasPrefix(out.Bytes, []byte("%PDF-")) {
		t.Errorf("not a PDF: %q", out.Bytes[:8])
	}
	if !strings.HasSuffix(out.Filename, ".pdf") {
		t.Errorf("filename %q should end in .pdf", out.Filename)
	}
	if out.ContentType != "application/pdf" {
		t.Errorf("content-type = %q", out.ContentType)
	}
	// Sanity floor: a real PDF with multiple sections is well above 2KB; a
	// trivially-empty doc is around 1KB. Catches an "early return" regression.
	if len(out.Bytes) < 2000 {
		t.Errorf("PDF unexpectedly small: %d bytes", len(out.Bytes))
	}
}

// TestExport_PeriodSummary_XLSX_HasSections verifies the new period_summary
// XLSX has the right sections, including critical stock, top productos and
// the daily ingresos vs egresos table.
func TestExport_PeriodSummary_XLSX_HasSections(t *testing.T) {
	reader := &fakeReader{
		incomeNow: 1500,
		expensesByCategory: map[string]float64{
			"renta": 1000,
		},
		topProducts: []reports.TopProductRow{
			{ProductID: uuid.New(), ProductName: "Proteina", Quantity: 3, Revenue: 900},
		},
		criticalStock: reports.CriticalStockCounts{OutCount: 1, LowCount: 0},
	}
	uc := newExportFixture(t, reader)
	out, err := uc.Execute(context.Background(), reports.ExportInput{
		GymID:  uuid.New(),
		Type:   reports.ReportTypePeriodSummary,
		Format: reports.FormatXLSX,
		Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(out.Bytes))
	if err != nil {
		t.Fatalf("xlsx open: %v", err)
	}
	defer f.Close()
	rows, err := f.GetRows("Sheet1")
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	// Flatten the rows we got into a single string so we can substring-match.
	var blob strings.Builder
	for _, r := range rows {
		for _, c := range r {
			blob.WriteString(c)
			blob.WriteString("\n")
		}
	}
	flat := blob.String()
	needles := []string{
		"Resumen del período", "Existencias bajas",
		"Indicadores", "Ingresos", "Salidas", "Resultado del período",
		"Compras pagadas", "Otros ingresos",
		"Ingresos y salidas por día", "Resultado diario",
		"Gastos por categoría", "Productos con más ingresos", "Proteina",
		"Gastos de operación del período",
	}
	for _, n := range needles {
		if !strings.Contains(flat, n) {
			t.Errorf("XLSX missing expected text %q", n)
		}
	}
	for _, legacy := range []string{"Resultado neto", "Flujo de caja", "Flujo diario"} {
		if strings.Contains(flat, legacy) {
			t.Errorf("XLSX still publishes legacy concept %q", legacy)
		}
	}
}

func TestExport_PeriodSummary_UsesOneSnapshotAndDoesNotDropRow201(t *testing.T) {
	expenses := make([]reports.ExpenseRow, 201)
	inventory := make([]reports.InventoryCostRow, 201)
	for i := range expenses {
		expenses[i] = reports.ExpenseRow{ID: uuid.New(), ExpenseDate: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Category: "otros", PaymentMethod: "transfer", Amount: 1}
		inventory[i] = reports.InventoryCostRow{MovementID: uuid.New(), ProductID: uuid.New(), ProductName: "Producto", Delta: 1, CostUnit: 1, CostTotal: 1, OccurredAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	}
	reader := &fakeReader{expenseRows: expenses, inventoryCostRows: inventory}
	uow := &snapshotSpyUoW{}
	gyms := &fakeGymRepo{gym: sampleGym()}
	rng := reports.NewRangeReport(reader, uow).WithGyms(gyms)
	uc := reports.NewExportReport(reader, gyms, uow, reports.NewAttentionRequired(reader, uow), rng)
	out, err := uc.Execute(context.Background(), reports.ExportInput{
		GymID: uuid.New(), Type: reports.ReportTypePeriodSummary, Format: reports.FormatXLSX, Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if uow.snapshots != 1 || uow.queries != 0 {
		t.Fatalf("snapshots=%d queries=%d, want one snapshot and no loose query", uow.snapshots, uow.queries)
	}
	if reader.inventoryListLimit != -1 || reader.expenseListLimit != -1 {
		t.Fatalf("export limits inventory/expenses=%d/%d, want unbounded sentinel -1", reader.inventoryListLimit, reader.expenseListLimit)
	}
	f, err := excelize.OpenReader(bytes.NewReader(out.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := f.GetRows("Sheet1")
	if err != nil {
		t.Fatal(err)
	}
	productRows := 0
	for _, row := range rows {
		if len(row) > 1 && row[0] == "2026-08-01" && row[1] == "Producto" {
			productRows++
		}
	}
	if productRows != 201 {
		t.Fatalf("exported inventory rows=%d, want all 201", productRows)
	}
}

// TestExport_PeriodSummary_CustomWindow_InFilename verifies the filename
// reflects the custom [from,to] passed in, not a defaulted month.
func TestExport_PeriodSummary_CustomWindow_InFilename(t *testing.T) {
	uc := newExportFixture(t, &fakeReader{})
	from := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 18, 0, 0, 0, 0, time.UTC)
	out, err := uc.Execute(context.Background(), reports.ExportInput{
		GymID:  uuid.New(),
		Type:   reports.ReportTypePeriodSummary,
		Format: reports.FormatPDF,
		Period: reports.PeriodCustom,
		From:   &from,
		To:     &to,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.Filename, "20260305") || !strings.Contains(out.Filename, "20260318") {
		t.Errorf("filename %q should embed custom window 20260305..20260318", out.Filename)
	}
}

func TestExport_PeriodSummaryRejectsInvalidWindowBeforeReading(t *testing.T) {
	uc := newExportFixture(t, &fakeReader{})
	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)
	cases := []struct {
		name string
		in   reports.ExportInput
		want error
	}{
		{name: "unknown period", in: reports.ExportInput{Period: "quarter"}, want: reports.ErrReportPeriodInvalid},
		{name: "custom missing bound", in: reports.ExportInput{Period: reports.PeriodCustom, From: &today}, want: reports.ErrCustomReportRangeRequired},
		{name: "preset missing bound", in: reports.ExportInput{Period: reports.PeriodMonth, From: &today}, want: reports.ErrCustomReportRangeRequired},
		{name: "custom reversed", in: reports.ExportInput{Period: reports.PeriodCustom, From: &tomorrow, To: &today}, want: reports.ErrReportRangeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.GymID = uuid.New()
			tc.in.Type = reports.ReportTypePeriodSummary
			tc.in.Format = reports.FormatXLSX
			_, err := uc.Execute(context.Background(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Execute() error = %v, want %v", err, tc.want)
			}
		})
	}
}

// A download may run after midnight, but must use the dates and comparison
// already shown to the owner, including calendar-month alignment.
func TestExport_PeriodSummaryFreezesDatesAndComparison(t *testing.T) {
	for _, tc := range []struct{ period, from, to, previousFrom, previousTo string }{
		{reports.PeriodMonth, "2026-09-01", "2026-09-28", "2026-08-01", "2026-08-28"},
		{reports.PeriodLastMonth, "2026-08-01", "2026-08-31", "2026-07-01", "2026-07-31"},
		{reports.PeriodMonth, "2026-03-01", "2026-03-31", "2026-02-01", "2026-02-28"},
		{reports.PeriodCustom, "2026-09-01", "2026-09-28", "2026-08-04", "2026-08-31"},
	} {
		t.Run(tc.period+tc.from, func(t *testing.T) {
			spy := &windowSpy{}
			rng := reports.NewRangeReport(spy, fakeUoW{})
			rng.Now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
			uc := reports.NewExportReport(spy, &fakeGymRepo{gym: sampleGym()}, fakeUoW{}, nil, rng)
			from, _ := time.Parse("2006-01-02", tc.from)
			to, _ := time.Parse("2006-01-02", tc.to)
			out, err := uc.Execute(context.Background(), reports.ExportInput{GymID: uuid.New(), Type: reports.ReportTypePeriodSummary, Format: reports.FormatXLSX, Period: tc.period, From: &from, To: &to})
			if err != nil {
				t.Fatal(err)
			}
			if len(spy.windows) < 2 {
				t.Fatalf("missing current/previous queries: %v", spy.windows)
			}
			for i, want := range [][2]string{{tc.from, tc.to}, {tc.previousFrom, tc.previousTo}} {
				got := [2]string{spy.windows[i][0].Format("2006-01-02"), spy.windows[i][1].Format("2006-01-02")}
				if got != want {
					t.Fatalf("query %d: got %v, want %v", i, got, want)
				}
			}
			f, err := excelize.OpenReader(bytes.NewReader(out.Bytes))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			rows, err := f.GetRows(f.GetSheetName(0))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				if len(row) >= 3 && row[0] == "Comparación" && row[1] == tc.previousFrom && row[2] == tc.previousTo {
					found = true
				}
			}
			if !found {
				t.Fatalf("download omits comparison %s..%s", tc.previousFrom, tc.previousTo)
			}
		})
	}
}
