package reports_test

import (
	"bytes"
	"context"
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
	gyms := &fakeGymRepo{gym: sampleGym()}
	return reports.NewExportReport(reader, gyms, fakeUoW{}, att)
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
