package recurring

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
)

func day(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }
func TestOccurrenceIDDeterministic(t *testing.T) {
	g := uuid.New()
	x := uuid.New()
	if OccurrenceID(g, x, day("2026-01-31")) != OccurrenceID(g, x, day("2026-01-31")) {
		t.Fatal("not deterministic")
	}
}
func TestNextDueClampsMonthEnd(t *testing.T) {
	got, _ := NextDue(day("2026-01-31"), Monthly, 31)
	if got.Format("2006-01-02") != "2026-02-28" {
		t.Fatal(got)
	}
	got, _ = NextDue(got, Monthly, 31)
	if got.Format("2006-01-02") != "2026-03-31" {
		t.Fatal(got)
	}
}
func TestSemimonthlyDiffersFromEvery14Days(t *testing.T) {
	a, _ := NextDue(day("2026-02-15"), Semimonthly, 15)
	b, _ := NextDue(day("2026-02-15"), Every14Days, 15)
	if a.Format("2006-01-02") != "2026-02-28" || b.Format("2006-01-02") != "2026-03-01" {
		t.Fatalf("%s %s", a, b)
	}
}
func TestAnnualLeapClamp(t *testing.T) {
	got, _ := NextDue(day("2028-02-29"), Annual, 29)
	if got.Format("2006-01-02") != "2029-02-28" {
		t.Fatal(got)
	}
}
func TestSchedulesCrossYearWithoutChangingBusinessDay(t *testing.T) {
	monthly, _ := NextDue(day("2026-12-31"), Monthly, 31)
	semi, _ := NextDue(day("2026-12-31"), Semimonthly, 31)
	if monthly.Format("2006-01-02") != "2027-01-31" || semi.Format("2006-01-02") != "2027-01-15" {
		t.Fatalf("monthly=%s semimonthly=%s", monthly, semi)
	}
}
func TestTemplateValidatesClassificationAndUnicodePayee(t *testing.T) {
	in := TemplateInput{ID: uuid.New(), GymID: uuid.New(), CreatedBy: uuid.New(), Name: "Nómina", Category: expenseDomain.CategoryPayroll, ExpectedAmount: 100, PaymentMethod: expenseDomain.PaymentTransfer, Classification: expenseDomain.ClassificationFixed, Frequency: Monthly, StartsOn: day("2026-01-31"), Now: time.Now()}
	if _, err := NewTemplate(in); err != nil {
		t.Fatalf("valid fixed template: %v", err)
	}
	in.Classification = "maybe"
	if _, err := NewTemplate(in); err == nil {
		t.Fatal("invalid classification accepted")
	}
	in.Classification = expenseDomain.ClassificationVariable
	payee := strings.Repeat("ñ", 121)
	in.PayeeName = &payee
	if _, err := NewTemplate(in); err == nil {
		t.Fatal("oversized Unicode payee accepted")
	}
}
func TestResolvedOccurrenceCannotChange(t *testing.T) {
	o := Occurrence{Status: Pending}
	e, a := uuid.New(), uuid.New()
	if err := o.MarkPaid(e, a, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := o.Skip(a, "x", time.Now()); err == nil {
		t.Fatal("wanted resolved error")
	}
}
