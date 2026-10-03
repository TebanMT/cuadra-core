package cashmovement_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	cashmovement "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
)

func TestCashInIsNonOperatingAndCannotBecomeExpense(t *testing.T) {
	now := time.Now().UTC()
	m, err := cashmovement.New(uuid.New(), uuid.New(), uuid.New(), now, 100,
		cashmovement.CashIn, "Fondo de cambio", now)
	if err != nil {
		t.Fatal(err)
	}
	if m.ClassificationStatus != cashmovement.NonOperating || !m.CanCorrectPhysical() {
		t.Fatalf("cash-in = %+v, want correctable non-operating movement", m)
	}
	expenseID := uuid.New()
	if err = m.Classify(&expenseID, cashmovement.AsExpense, now.Add(time.Second)); !errors.Is(err, expErrors.ErrCashInCannotBeClassified) {
		t.Fatalf("classify cash-in as expense err=%v", err)
	}
	if err = m.ClassifyInventoryPurchase(now.Add(time.Second)); !errors.Is(err, expErrors.ErrAlreadyClassified) {
		t.Fatalf("classify cash-in as inventory purchase err=%v", err)
	}
	if m.ExpenseID != nil || m.ClassificationStatus != cashmovement.NonOperating {
		t.Fatalf("failed classification mutated cash-in: %+v", m)
	}
}

func TestPhysicalTypeCorrectionNormalizesClassification(t *testing.T) {
	now := time.Now().UTC()
	m, err := cashmovement.New(uuid.New(), uuid.New(), uuid.New(), now, 75,
		cashmovement.CashOut, "Salida capturada", now)
	if err != nil {
		t.Fatal(err)
	}
	if m.ClassificationStatus != cashmovement.Unclassified {
		t.Fatalf("cash-out status=%q, want unclassified", m.ClassificationStatus)
	}
	if err = m.Update(now, 75, cashmovement.CashIn, "Era fondo de cambio", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if m.ClassificationStatus != cashmovement.NonOperating || !m.CanCorrectPhysical() {
		t.Fatalf("corrected cash-in = %+v, want correctable non-operating", m)
	}
	if err = m.Update(now, 75, cashmovement.CashOut, "Era una salida", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if m.ClassificationStatus != cashmovement.Unclassified || !m.CanCorrectPhysical() {
		t.Fatalf("corrected cash-out = %+v, want correctable unclassified", m)
	}
}

func TestUnclassifyLegacyCashInRepairsToNonOperating(t *testing.T) {
	now := time.Now().UTC()
	expenseID := uuid.New()
	m := &cashmovement.CashMovement{
		ID: uuid.New(), GymID: uuid.New(), MovementType: cashmovement.CashIn,
		ClassificationStatus: cashmovement.AsExpense, ExpenseID: &expenseID,
		Version: 2, UpdatedAt: now,
	}
	m.Unclassify(now.Add(time.Second))
	if m.ClassificationStatus != cashmovement.NonOperating || m.ExpenseID != nil {
		t.Fatalf("legacy cash-in repair = %+v, want non-operating without expense", m)
	}
}
