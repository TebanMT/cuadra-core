package reports

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
)

func strPtr(s string) *string { return &s }

func periodCashFixture() *RangeReportOutput {
	expected, counted := 300.0, 250.0
	ana, caja2 := "Ana", "Caja 2"
	return &RangeReportOutput{CashReconciliation: CashReconciliation{
		Status: "incomplete", RequiresAttention: true,
		PeriodActivity: 200, PeriodWithdrawn: 700, WithdrawnSessions: 2,
		LatestExpected: &expected, LatestCounted: &counted, LatestStatus: "stale", LatestNeedsRecount: true,
		ShortageTotal: 50, ShortageSessions: 1, SurplusTotal: 0, SurplusSessions: 0, CountedCloses: 3,
		DifferenceSessions: []CashSessionDifference{
			{Date: "2026-11-12", DrawerName: &caja2, Sequence: 2, Difference: -50, CountedByName: &ana, Reason: strPtr("no había cambio")},
		},
		OpeningShortageTotal: 20, OpeningShortageSessions: 1,
		OpeningDifferences: []CashOpeningDifference{
			{Date: "2026-11-13", Difference: -20, OpenedByName: &ana, Note: strPtr("Faltaban 20 al llegar")},
		},
		UncoveredActivityDays: 2, UncoveredActivityDates: []string{"2026-11-03", "2026-11-05"},
		ClosedUnverifiedSessions: 1, UnknownOpeningSessions: 2, StaleSessions: 1,
		ActiveSessions: 5, ReconciledSessions: 3,
	}}
}

func TestPeriodCashExportSpeaksTheOwnersLanguage(t *testing.T) {
	data, err := renderPeriodSummaryXLSX(periodCashFixture(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	rows, err := f.GetRows(sheetName)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	find := func(label string) []string {
		t.Helper()
		for _, r := range rows {
			if len(r) > 0 && r[0] == label {
				return r
			}
		}
		t.Fatalf("missing row %q in %v", label, rows)
		return nil
	}
	cells := func(r []string) string { return strings.Join(r, " | ") }

	if got := cells(find("Caja del período")); got != "Caja del período | Volver a contar" {
		t.Errorf("status row = %q", got)
	}
	if got := cells(find("Entró a la caja")); got != "Entró a la caja | 200 | Cobros en efectivo, menos gastos y devoluciones pagados de la caja" {
		t.Errorf("in row = %q", got)
	}
	if got := cells(find("Se retiró de la caja")); got != "Se retiró de la caja | 700 | En 2 retiros · incluye efectivo que ya estaba al abrir" {
		t.Errorf("withdrawn row = %q", got)
	}
	if got := cells(find("Faltó en los cortes")); got != "Faltó en los cortes | 50 | En 1 corte" {
		t.Errorf("shortage row = %q", got)
	}
	if got := cells(find("Sobró en los cortes")); got != "Sobró en los cortes | 0 | Ningún corte con sobrante" {
		t.Errorf("surplus row = %q", got)
	}
	if got := cells(find("Faltó al abrir la caja")); got != "Faltó al abrir la caja | 20 | En 1 apertura" {
		t.Errorf("opening row = %q", got)
	}
	// Days without a finished count come first among the pending items.
	pending := find("Falta completar la caja")
	start := -1
	for i, r := range rows {
		if len(r) > 0 && r[0] == pending[0] {
			start = i
		}
	}
	wantPending := []string{
		"2 días sin corte terminado | 2026-11-03, 2026-11-05",
		"1 corte se cerró sin contar el efectivo",
		"En 2 cortes no se anotó con cuánto efectivo se empezó",
		"Hay que volver a contar 1 corte",
	}
	for i, want := range wantPending {
		if got := cells(rows[start+1+i]); got != want {
			t.Errorf("pending[%d] = %q, want %q", i, got, want)
		}
	}
	if got := cells(find("2026-11-12")); got != "2026-11-12 | Corte 2 | Caja 2 | Faltan $50.00 | -50 | Contó Ana · no había cambio" {
		t.Errorf("difference row = %q", got)
	}
	if got := cells(find("2026-11-13")); got != "2026-11-13 | Al abrir |  | Faltan $20.00 | -20 | Abrió Ana · Faltaban 20 al llegar" {
		t.Errorf("opening difference row = %q", got)
	}
	if got := cells(find("Diferencia del último corte")); got != "Diferencia del último corte | Hubo movimientos después de contar" {
		t.Errorf("latest difference row = %q", got)
	}
	find("5 cortes en el período · 3 contados · 2 con retiro · 1 cerrado sin contar · 1 por volver a contar")

	for _, r := range rows {
		text := strings.ToLower(cells(r))
		for _, jargon := range []string{"sesión", "sesiones", "concilia", "cobertura", "actividad de caja", "último conteo"} {
			if strings.Contains(text, jargon) {
				t.Errorf("row %q still says %q", cells(r), jargon)
			}
		}
	}
}

func TestPeriodCashExportSaysWhenMoreCashLeftThanCameIn(t *testing.T) {
	section := buildPeriodCashExport(CashReconciliation{Status: "complete", Complete: true, PeriodActivity: -120, ActiveSessions: 1, ReconciledSessions: 1})
	if section.Status != "Caja revisada" {
		t.Errorf("status = %q", section.Status)
	}
	in := section.Answers[0]
	if in.Label != "Salió de la caja" || in.Amount == nil || *in.Amount != 120 {
		t.Errorf("first answer = %+v", in)
	}
	if w := section.Answers[1]; w.Detail != "Sin retiros registrados" {
		t.Errorf("withdrawn = %+v", w)
	}
	if len(section.Pending) != 0 || len(section.Differences) != 0 || len(section.Latest) != 0 {
		t.Errorf("nothing pending expected: %+v", section)
	}
	uncounted := buildPeriodCashExport(CashReconciliation{Status: "incomplete", RequiresAttention: true, OpenSessions: 1, ActiveSessions: 1})
	if s := uncounted.Answers[2]; s.Amount != nil || s.Text != "Sin cortes contados" {
		t.Errorf("shortage without counts must not claim $0: %+v", s)
	}
}

func TestPeriodCashPDFRendersEveryBlock(t *testing.T) {
	out := renderPeriodSummaryPDF(&gymDomain.Gym{}, periodCashFixture(), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))
	if !bytes.HasPrefix(out, []byte("%PDF-")) || len(out) < 2000 {
		t.Fatalf("unexpected PDF (%d bytes)", len(out))
	}
}
