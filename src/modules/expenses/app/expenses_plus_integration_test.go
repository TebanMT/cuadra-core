//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	cashclose "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingRepos "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	recurring "github.com/cuadra/cuadra-core/src/modules/expenses/domain/recurring"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	repos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchaseDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	prodRepos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type failingExpenseAudit struct{}

func (failingExpenseAudit) Record(context.Context, sharedDomain.Transaction, audit.Entry) error {
	return errors.New("audit unavailable")
}

type cashAdjustmentSpy struct {
	calls int
	input expApp.CashSessionAdjustmentInput
}

func (s *cashAdjustmentSpy) MarkPhysicalCashAdjustment(_ context.Context, _ sharedDomain.Transaction, in expApp.CashSessionAdjustmentInput) error {
	s.calls++
	s.input = in
	return nil
}

func applyCashInWriteBarrier(t *testing.T, f *expensesFixture) {
	t.Helper()
	schema, err := os.ReadFile("../../../../db_migrations/sqlite/041_cash_in_non_operating.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(string(schema)); err != nil {
		t.Fatalf("apply cash-in write barrier: %v", err)
	}
}

func mexicoBusinessDay(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		t.Fatalf("load gym timezone: %v", err)
	}
	now := time.Now().In(loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

func customDrawer(t *testing.T, f *expensesFixture, name, key string) (*billingApp.CashDrawers, *cashclose.CashDrawer) {
	t.Helper()
	catalog := billingApp.NewCashDrawers(billingRepos.NewCashDrawerSQLiteRepository(), f.uow, f.recorder)
	drawer, err := catalog.Create(context.Background(), billingApp.CreateCashDrawerInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", Name: name, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("create drawer: %v", err)
	}
	return catalog, drawer
}

func TestInventoryPurchaseCash_UsesCompatibleUnclassifiedOutflowWithoutDuplicatingCash(t *testing.T) {
	f := setupExpenses(t)
	movements := repos.NewCashMovementSQLiteRepository()
	catalog := billingApp.NewCashDrawers(billingRepos.NewCashDrawerSQLiteRepository(), f.uow, f.recorder)
	service := expApp.NewInventoryPurchaseCashService(movements).WithCashDrawerValidator(catalog).WithAudit(f.recorder)
	day := mexicoBusinessDay(t)
	movementID := uuid.New()
	now := time.Now().UTC()
	err := f.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		m, newErr := cashDomain.New(movementID, f.gymID, f.ownerID, day, 750,
			cashDomain.CashOut, "Pago al proveedor", now)
		if newErr != nil {
			return newErr
		}
		if _, newErr = movements.Create(tx, m); newErr != nil {
			return newErr
		}
		drawerID, linkErr := service.UseExistingInventoryPurchaseCash(context.Background(), tx,
			movementID, f.gymID, f.ownerID, nil, day, 750, now.Add(time.Second))
		if linkErr != nil {
			return linkErr
		}
		if drawerID != f.gymID {
			t.Fatalf("drawer=%s want main=%s", drawerID, f.gymID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var count, version int
	var status string
	if err = f.db.Get(&count, `SELECT COUNT(*) FROM cash_movements WHERE gym_id=? AND deleted_at IS NULL`, f.gymID.String()); err != nil {
		t.Fatal(err)
	}
	if err = f.db.QueryRowx(`SELECT version,classification_status FROM cash_movements WHERE id=?`, movementID.String()).Scan(&version, &status); err != nil {
		t.Fatal(err)
	}
	if count != 1 || version != 2 || status != cashDomain.AsInventoryPurchase {
		t.Fatalf("count=%d version=%d status=%s, want 1/2/%s", count, version, status, cashDomain.AsInventoryPurchase)
	}
}

func TestInventoryPurchaseCash_RejectsMismatchedOutflowAndKeepsItUnclassified(t *testing.T) {
	f := setupExpenses(t)
	movements := repos.NewCashMovementSQLiteRepository()
	service := expApp.NewInventoryPurchaseCashService(movements)
	day := mexicoBusinessDay(t)
	movementID := uuid.New()
	now := time.Now().UTC()
	err := f.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		m, newErr := cashDomain.New(movementID, f.gymID, f.ownerID, day, 500,
			cashDomain.CashOut, "Salida previa", now)
		if newErr != nil {
			return newErr
		}
		if _, newErr = movements.Create(tx, m); newErr != nil {
			return newErr
		}
		_, linkErr := service.UseExistingInventoryPurchaseCash(context.Background(), tx,
			movementID, f.gymID, f.ownerID, nil, day, 501, now.Add(time.Second))
		if !errors.Is(linkErr, expErrors.ErrCashMovementNotCompatible) {
			t.Fatalf("link err=%v, want mismatch", linkErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err = f.db.Get(&status, `SELECT classification_status FROM cash_movements WHERE id=?`, movementID.String()); err != nil {
		t.Fatal(err)
	}
	if status != cashDomain.Unclassified {
		t.Fatalf("status=%s, want unclassified", status)
	}
}

func TestInventoryPurchaseCorrection_IsAppendOnlyIdempotentAndPreservesPayment(t *testing.T) {
	f := setupExpenses(t)
	products := prodRepos.NewProductSQLiteRepository()
	stock := prodRepos.NewStockMovementSQLiteRepository()
	purchases := prodRepos.NewInventoryPurchaseSQLiteRepository()
	cash := expApp.NewInventoryPurchaseCashService(repos.NewCashMovementSQLiteRepository())
	created, err := prodApp.NewCreateProduct(products, stock, f.uow, f.recorder).Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Proteína", Price: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	cost := 5.0
	isPurchase := true
	restock, err := prodApp.NewAdjustStock(products, stock, f.uow, f.recorder).
		WithPurchases(purchases, cash).
		Execute(context.Background(), prodApp.AdjustStockInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "operator", ProductID: created.ProductID,
			MovementType: "restock", Quantity: 10, Cost: &cost, IsPurchase: &isPurchase,
			PurchaseStatus: purchaseDomain.StatusUnpaid, IdempotencyKey: "purchase:protein:initial",
		})
	if err != nil || restock.PurchaseID == nil {
		t.Fatalf("restock=%+v err=%v", restock, err)
	}
	correct := prodApp.NewCorrectInventoryPurchase(purchases, products, stock, f.uow, f.recorder)
	newCost := 7.0
	in := prodApp.CorrectInventoryPurchaseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PurchaseID: *restock.PurchaseID,
		ExpectedVersion: 1, Quantity: 6, UnitCost: &newCost,
		CorrectionReason: "el proveedor entregó seis piezas", IdempotencyKey: "purchase:protein:correction:1",
	}
	first, err := correct.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := correct.Execute(context.Background(), in)
	if err != nil || second.Version != first.Version || second.NewStock != first.NewStock ||
		len(second.CorrectionMovementIDs) != 2 || second.CorrectionMovementIDs[0] != first.CorrectionMovementIDs[0] {
		t.Fatalf("exact retry first=%+v second=%+v err=%v", first, second, err)
	}
	if first.Quantity != 6 || first.TotalAmount == nil || *first.TotalAmount != 42 || first.NewStock != 6 || first.StockDelta != -4 {
		t.Fatalf("correction=%+v", first)
	}
	var movementRows, purchaseQty, productStock int
	var purchaseTotal int64
	if err = f.db.Get(&movementRows, `SELECT COUNT(*) FROM stock_movements WHERE product_id=? AND deleted_at IS NULL`, created.ProductID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.QueryRowx(`SELECT quantity,total_amount FROM inventory_purchases WHERE id=?`, *restock.PurchaseID).Scan(&purchaseQty, &purchaseTotal); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&productStock, `SELECT stock FROM products WHERE id=?`, created.ProductID); err != nil {
		t.Fatal(err)
	}
	if movementRows != 3 || purchaseQty != 6 || purchaseTotal != 4200 || productStock != 6 {
		t.Fatalf("movements=%d qty=%d total=%d stock=%d", movementRows, purchaseQty, purchaseTotal, productStock)
	}
	tx := mustQuery(t, f.uow)
	avg, err := products.GetUnitCost(tx, f.gymID, created.ProductID)
	if err != nil || avg == nil || *avg != 7 {
		t.Fatalf("corrected average=%v err=%v", avg, err)
	}
	in.Quantity = 7
	if _, err = correct.Execute(context.Background(), in); !errors.Is(err, prodErrors.ErrPurchaseCorrectionConflict) {
		t.Fatalf("changed idempotent retry err=%v", err)
	}

	paid, err := prodApp.NewPayInventoryPurchase(purchases, products, cash, f.uow, f.recorder).Execute(context.Background(), prodApp.PayInventoryPurchaseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PurchaseID: *restock.PurchaseID,
		ExpectedVersion: first.Version, PaidOn: mexicoBusinessDay(t), PaymentMethod: purchaseDomain.MethodTransfer,
		PaidFrom: purchaseDomain.PaidFromGymFund, IdempotencyKey: "purchase:protein:payment",
	})
	if err != nil {
		t.Fatal(err)
	}
	newCost = 8
	correctedPaid, err := correct.Execute(context.Background(), prodApp.CorrectInventoryPurchaseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PurchaseID: *restock.PurchaseID,
		ExpectedVersion: paid.Purchase.Version, Quantity: 6, UnitCost: &newCost,
		CorrectionReason: "costo capturado incorrectamente", IdempotencyKey: "purchase:protein:paid-correction",
	})
	if err != nil || correctedPaid.Status != purchaseDomain.StatusPaid || correctedPaid.TotalAmount == nil || *correctedPaid.TotalAmount != 48 || correctedPaid.PaidOn == nil {
		t.Fatalf("paid correction=%+v err=%v", correctedPaid, err)
	}

	annulled, err := correct.Execute(context.Background(), prodApp.CorrectInventoryPurchaseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PurchaseID: *restock.PurchaseID,
		ExpectedVersion: correctedPaid.Version, Annul: true,
		CorrectionReason: "la entrega nunca ocurrió", IdempotencyKey: "purchase:protein:annul",
	})
	if err != nil || !annulled.Annulled || annulled.Status != purchaseDomain.StatusAnnulled || annulled.NewStock != 0 {
		t.Fatalf("annulled=%+v err=%v", annulled, err)
	}
	tx = mustQuery(t, f.uow)
	avg, err = products.GetUnitCost(tx, f.gymID, created.ProductID)
	if err != nil || avg != nil {
		t.Fatalf("annulled average=%v err=%v", avg, err)
	}
	var legacyCorrectionRows int
	if err = f.db.Get(&legacyCorrectionRows, `SELECT COUNT(*) FROM stock_movements WHERE product_id=? AND id<>? AND is_purchase=1`, created.ProductID, restock.MovementID); err != nil {
		t.Fatal(err)
	}
	if legacyCorrectionRows != 0 {
		t.Fatalf("correction journal rows marked as purchases=%d", legacyCorrectionRows)
	}
}

func TestInventoryPurchaseList_HasExplicitStatusDateAndPagination(t *testing.T) {
	f := setupExpenses(t)
	products := prodRepos.NewProductSQLiteRepository()
	stock := prodRepos.NewStockMovementSQLiteRepository()
	purchases := prodRepos.NewInventoryPurchaseSQLiteRepository()
	cash := expApp.NewInventoryPurchaseCashService(repos.NewCashMovementSQLiteRepository())
	created, err := prodApp.NewCreateProduct(products, stock, f.uow, f.recorder).Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Bebida", Price: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	isPurchase := true
	for i := 1; i <= 3; i++ {
		cost := float64(i)
		if _, err = prodApp.NewAdjustStock(products, stock, f.uow, f.recorder).WithPurchases(purchases, cash).
			Execute(context.Background(), prodApp.AdjustStockInput{
				GymID: f.gymID, ActorUserID: f.ownerID, ProductID: created.ProductID,
				MovementType: "restock", Quantity: i, Cost: &cost, IsPurchase: &isPurchase,
				PurchaseStatus: purchaseDomain.StatusUnpaid, IdempotencyKey: "purchase:list:" + strconv.Itoa(i),
			}); err != nil {
			t.Fatal(err)
		}
	}
	list := prodApp.NewListInventoryPurchases(purchases, products, cash, f.uow)
	today := time.Now().UTC()
	from := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	page, err := list.ExecuteList(context.Background(), prodApp.ListInventoryPurchasesInput{
		GymID: f.gymID, Status: "all", From: &from, To: &from, Page: 2, PageSize: 1,
	})
	if err != nil || page.Total != 3 || len(page.Items) != 1 || page.Page != 2 || page.PageSize != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	paid, err := list.ExecuteList(context.Background(), prodApp.ListInventoryPurchasesInput{
		GymID: f.gymID, Status: purchaseDomain.StatusPaid, Page: 1, PageSize: 50,
	})
	if err != nil || paid.Total != 0 || len(paid.Items) != 0 {
		t.Fatalf("paid=%+v err=%v", paid, err)
	}
}

func TestInventoryPurchasePaymentsRejectFutureLocalDayAndAllowBackdate(t *testing.T) {
	f := setupExpenses(t)
	products := prodRepos.NewProductSQLiteRepository()
	stock := prodRepos.NewStockMovementSQLiteRepository()
	purchases := prodRepos.NewInventoryPurchaseSQLiteRepository()
	cash := expApp.NewInventoryPurchaseCashService(repos.NewCashMovementSQLiteRepository())
	gyms := gymRepoLite.NewGymSQLiteRepository()
	created, err := prodApp.NewCreateProduct(products, stock, f.uow, f.recorder).Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Producto con fecha", Price: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	cost, isPurchase := 5.0, true
	today := mexicoBusinessDay(t)
	future := today.AddDate(0, 0, 1)
	backdate := today.AddDate(0, 0, -1)
	adjust := prodApp.NewAdjustStock(products, stock, f.uow, f.recorder).
		WithPurchases(purchases, cash).WithGyms(gyms)
	paidInput := prodApp.AdjustStockInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", ProductID: created.ProductID,
		MovementType: "restock", Quantity: 2, Cost: &cost, IsPurchase: &isPurchase,
		PurchaseStatus: purchaseDomain.StatusPaid, PaidOn: &future,
		PaymentMethod: purchaseDomain.MethodTransfer, PaidFrom: purchaseDomain.PaidFromGymFund,
		IdempotencyKey: "purchase:future-immediate",
	}
	if _, err = adjust.Execute(context.Background(), paidInput); !errors.Is(err, prodErrors.ErrPurchaseDateInvalid) {
		t.Fatalf("future immediate purchase error=%v", err)
	}
	var stockAfter, purchaseCount int
	if err = f.db.Get(&stockAfter, `SELECT stock FROM products WHERE id=?`, created.ProductID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&purchaseCount, `SELECT COUNT(*) FROM inventory_purchases WHERE idempotency_key=?`, paidInput.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if stockAfter != 0 || purchaseCount != 0 {
		t.Fatalf("rejected future purchase changed stock/purchases=%d/%d", stockAfter, purchaseCount)
	}
	paidInput.PaidOn = &backdate
	paid, err := adjust.Execute(context.Background(), paidInput)
	if err != nil || paid.PurchaseID == nil {
		t.Fatalf("backdated immediate purchase=%+v err=%v", paid, err)
	}
	if _, err = adjust.Execute(context.Background(), func() prodApp.AdjustStockInput {
		retry := paidInput
		retry.PaidOn = &future
		return retry
	}()); !errors.Is(err, prodErrors.ErrPurchaseDateInvalid) {
		t.Fatalf("future retry must fail before replay/conflict, got %v", err)
	}

	unpaid, err := adjust.Execute(context.Background(), prodApp.AdjustStockInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ProductID: created.ProductID,
		MovementType: "restock", Quantity: 1, Cost: &cost, IsPurchase: &isPurchase,
		PurchaseStatus: purchaseDomain.StatusUnpaid, IdempotencyKey: "purchase:pending-for-date",
	})
	if err != nil || unpaid.PurchaseID == nil {
		t.Fatalf("pending purchase=%+v err=%v", unpaid, err)
	}
	pay := prodApp.NewPayInventoryPurchase(purchases, products, cash, f.uow, f.recorder).WithGyms(gyms)
	payInput := prodApp.PayInventoryPurchaseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PurchaseID: *unpaid.PurchaseID,
		ExpectedVersion: 1, PaidOn: future, PaymentMethod: purchaseDomain.MethodTransfer,
		PaidFrom: purchaseDomain.PaidFromGymFund, IdempotencyKey: "purchase:future-pending-payment",
	}
	if _, err = pay.Execute(context.Background(), payInput); !errors.Is(err, prodErrors.ErrPurchaseDateInvalid) {
		t.Fatalf("future pending payment error=%v", err)
	}
	var status string
	if err = f.db.Get(&status, `SELECT status FROM inventory_purchases WHERE id=?`, *unpaid.PurchaseID); err != nil {
		t.Fatal(err)
	}
	if status != purchaseDomain.StatusUnpaid {
		t.Fatalf("future payment changed status=%s", status)
	}
	payInput.PaidOn = backdate
	settled, err := pay.Execute(context.Background(), payInput)
	if err != nil || settled.Purchase.PaidOn == nil || !settled.Purchase.PaidOn.Equal(backdate) {
		t.Fatalf("backdated pending payment=%+v err=%v", settled, err)
	}
	payInput.PaidOn = future
	if _, err = pay.Execute(context.Background(), payInput); !errors.Is(err, prodErrors.ErrPurchaseDateInvalid) {
		t.Fatalf("future resolved retry must fail before replay, got %v", err)
	}
}

func TestCreateExpense_CustomDrawerAndIdempotencyAreExact(t *testing.T) {
	f := setupExpenses(t)
	catalog, drawer := customDrawer(t, f, "Recepción norte", "drawer:north")
	moves := repos.NewCashMovementSQLiteRepository()
	uc := expApp.NewCreateExpense(f.expenseRepo, f.uow, f.recorder).
		WithOperational(moves, expApp.OperationalPolicy{}).
		WithCashDrawerValidator(catalog)
	day := mexicoBusinessDay(t)
	description := "Limpieza"
	in := expApp.CreateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day, Amount: 350,
		Category: expenseDomain.CategoryMaintenance, Description: &description,
		PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister,
		CashDrawerID: &drawer.ID, IdempotencyKey: "expense:cleaning:2026-08-24",
	}
	first, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := uc.Execute(context.Background(), in)
	if err != nil || second.ExpenseID != first.ExpenseID {
		t.Fatalf("exact retry: first=%s second=%v err=%v", first.ExpenseID, second, err)
	}
	var count int
	if err = f.db.Get(&count, `SELECT COUNT(*) FROM expenses WHERE id=?`, first.ExpenseID); err != nil || count != 1 {
		t.Fatalf("expense count=%d err=%v", count, err)
	}
	var storedDrawer string
	if err = f.db.Get(&storedDrawer, `SELECT cash_drawer_id FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, first.ExpenseID); err != nil || storedDrawer != drawer.ID.String() {
		t.Fatalf("drawer=%q want=%s err=%v", storedDrawer, drawer.ID, err)
	}
	in.Amount = 351
	if _, err = uc.Execute(context.Background(), in); !errors.Is(err, expErrors.ErrIdempotencyKeyConflict) {
		t.Fatalf("changed retry err=%v, want conflict", err)
	}
}

func TestCreateExpense_RejectsInactiveDrawer(t *testing.T) {
	f := setupExpenses(t)
	catalog, drawer := customDrawer(t, f, "Caja temporal", "drawer:temporary")
	if _, err := catalog.Deactivate(context.Background(), f.gymID, f.ownerID, "owner", drawer.ID, drawer.Version); err != nil {
		t.Fatal(err)
	}
	_, err := expApp.NewCreateExpense(f.expenseRepo, f.uow, f.recorder).
		WithOperational(repos.NewCashMovementSQLiteRepository(), expApp.OperationalPolicy{}).
		WithCashDrawerValidator(catalog).
		Execute(context.Background(), expApp.CreateExpenseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: mexicoBusinessDay(t), Amount: 100,
			Category: expenseDomain.CategoryOther, PaymentMethod: expenseDomain.PaymentCash,
			PaidFrom: expenseDomain.PaidFromCashRegister, CashDrawerID: &drawer.ID,
			IdempotencyKey: "expense:inactive-drawer",
		})
	if !errors.Is(err, cashclose.ErrDrawerInactive) {
		t.Fatalf("err=%v, want inactive drawer", err)
	}
}

func TestOccurrencePayment_CustomDrawerRetryRejectsChangedPayload(t *testing.T) {
	f := setupExpenses(t)
	catalog, drawer := customDrawer(t, f, "Mostrador", "drawer:counter")
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	moves := repos.NewCashMovementSQLiteRepository()
	day := mexicoBusinessDay(t)
	if _, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(context.Background(), expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Limpieza", Category: expenseDomain.CategoryMaintenance,
		ExpectedAmount: 600, PaymentMethod: expenseDomain.PaymentCash,
		Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: day,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(context.Background(), f.gymID, day); err != nil {
		t.Fatal(err)
	}
	rows, err := expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("occurrences=%d err=%v", len(rows), err)
	}
	pay := expApp.NewMarkExpenseOccurrencePaid(occurrences, f.expenseRepo, moves, f.uow, f.recorder, expApp.OperationalPolicy{}).
		WithCashDrawerValidator(catalog)
	in := expApp.MarkOccurrencePaidInput{
		GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID,
		PaidOn: day, Amount: 600, PaymentMethod: expenseDomain.PaymentCash,
		PaidFrom: expenseDomain.PaidFromCashRegister, CashDrawerID: &drawer.ID,
		IdempotencyKey: "occurrence:cleaning:paid",
	}
	first, err := pay.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pay.Execute(context.Background(), in); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	var storedDrawer string
	if err = f.db.Get(&storedDrawer, `SELECT cash_drawer_id FROM cash_movements WHERE expense_id=?`, first.ID); err != nil || storedDrawer != drawer.ID.String() {
		t.Fatalf("drawer=%q want=%s err=%v", storedDrawer, drawer.ID, err)
	}
	in.Amount = 650
	if _, err = pay.Execute(context.Background(), in); !errors.Is(err, expErrors.ErrIdempotencyKeyConflict) {
		t.Fatalf("changed retry err=%v, want conflict", err)
	}
}

func TestExpensesPlus_OccurrencePaymentIsIdempotentAndCashOnce(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	moves := repos.NewCashMovementSQLiteRepository()
	lock := repos.NewCashDayLockSQLite()
	policy := expApp.OperationalPolicy{DayLock: lock}
	day := mexicoBusinessDay(t)
	createT := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder)
	tpl, err := createT.Execute(context.Background(), expApp.RecurringTemplateInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: "Renta", Category: expenseDomain.CategoryRent, ExpectedAmount: 5000, PaymentMethod: expenseDomain.PaymentCash, Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: day})
	if err != nil {
		t.Fatal(err)
	}
	materialize := expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder)
	if n, err := materialize.Execute(context.Background(), f.gymID, day); err != nil || n != 1 {
		t.Fatalf("materialize n=%d err=%v", n, err)
	}
	list := expApp.NewListExpenseOccurrences(occurrences, f.uow)
	rows, err := list.Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	pay := expApp.NewMarkExpenseOccurrencePaid(occurrences, f.expenseRepo, moves, f.uow, f.recorder, policy)
	in := expApp.MarkOccurrencePaidInput{GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID, PaidOn: day, Amount: 5100, PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister}
	first, err := pay.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pay.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("expense duplicated: %s %s", first.ID, second.ID)
	}
	var expenses, movements int
	_ = f.db.Get(&expenses, `SELECT COUNT(*) FROM expenses WHERE recurring_occurrence_id=? AND deleted_at IS NULL`, rows[0].ID)
	_ = f.db.Get(&movements, `SELECT COUNT(*) FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, first.ID)
	if expenses != 1 || movements != 1 {
		t.Fatalf("expenses=%d movements=%d template=%s", expenses, movements, tpl.ID)
	}
	var stagedTypes int
	if err := f.db.Get(&stagedTypes, `SELECT COUNT(DISTINCT entity_type) FROM sync_queue WHERE entity_type IN ('recurring_expense_templates','expense_occurrences','cash_movements','expenses') AND synced_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if stagedTypes != 4 {
		t.Fatalf("staged Expenses Plus entity types=%d, want 4", stagedTypes)
	}
}

func TestExpensesPlus_TransferAffectsNoDrawer(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	moves := repos.NewCashMovementSQLiteRepository()
	policy := expApp.OperationalPolicy{DayLock: repos.NewCashDayLockSQLite()}
	day := mexicoBusinessDay(t)
	if _, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(context.Background(), expApp.RecurringTemplateInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: "Internet", Category: expenseDomain.CategoryUtilities, ExpectedAmount: 800, PaymentMethod: expenseDomain.PaymentTransfer, Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: day}); err != nil {
		t.Fatal(err)
	}
	// Use the public materialization path instead of inserting test-only state.
	_, err := expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(context.Background(), f.gymID, day)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
	e, err := expApp.NewMarkExpenseOccurrencePaid(occurrences, f.expenseRepo, moves, f.uow, f.recorder, policy).Execute(context.Background(), expApp.MarkOccurrencePaidInput{GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID, PaidOn: day, Amount: 800, PaymentMethod: expenseDomain.PaymentTransfer, PaidFrom: expenseDomain.PaidFromGymFund})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.db.Get(&n, `SELECT COUNT(*) FROM cash_movements WHERE expense_id=?`, e.ID)
	if n != 0 {
		t.Fatalf("transfer created %d drawer movements", n)
	}
}

func TestExpensesPlus_RecurringPaymentPreservesVariableClassification(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	moves := repos.NewCashMovementSQLiteRepository()
	policy := expApp.OperationalPolicy{DayLock: repos.NewCashDayLockSQLite()}
	day := mexicoBusinessDay(t)
	if _, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(context.Background(), expApp.RecurringTemplateInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: "Publicidad", Category: expenseDomain.CategoryMarketing, ExpectedAmount: 1250, PaymentMethod: expenseDomain.PaymentTransfer, Classification: expenseDomain.ClassificationVariable, Frequency: recurring.Monthly, StartsOn: day}); err != nil {
		t.Fatal(err)
	}
	if _, err := expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(context.Background(), f.gymID, day); err != nil {
		t.Fatal(err)
	}
	rows, err := expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	e, err := expApp.NewMarkExpenseOccurrencePaid(occurrences, f.expenseRepo, moves, f.uow, f.recorder, policy).Execute(context.Background(), expApp.MarkOccurrencePaidInput{GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID, PaidOn: day, Amount: 1250, PaymentMethod: expenseDomain.PaymentTransfer, PaidFrom: expenseDomain.PaidFromGymFund})
	if err != nil {
		t.Fatal(err)
	}
	if e.Classification != expenseDomain.ClassificationVariable {
		t.Fatalf("classification=%q, want variable", e.Classification)
	}
}

func TestExpensesPlus_ClosedDayAcceptsLateMovement(t *testing.T) {
	f := setupExpenses(t)
	day := mexicoBusinessDay(t).Format("2006-01-02")
	now := time.Now().UTC().UnixMilli()
	_, err := f.db.Exec(`INSERT INTO cash_close_events(
		id,gym_id,version,created_at,updated_at,close_date,calculated_cash,closed_by,
		drawer_id,drawer_code,operational_date,sequence,status,opening_cash,
		opening_cash_known,activity_cash,adjusted_after_withdrawal,opened_at,opened_by,closed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,1,'closed_unverified',0,1,0,0,?,?,?)`,
		uuid.New(), f.gymID, 1, now, now, day, 0, f.ownerID,
		f.gymID, "main", day, now, f.ownerID, now)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := time.Parse("2006-01-02", day)
	uc := expApp.NewCreateCashMovement(repos.NewCashMovementSQLiteRepository(), f.uow, f.recorder, expApp.OperationalPolicy{DayLock: repos.NewCashDayLockSQLite()})
	m, err := uc.Execute(context.Background(), expApp.CashMovementInput{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: d, Amount: 100, MovementType: cashDomain.CashOut, Reason: "Cambio"})
	if err != nil || m == nil {
		t.Fatalf("late physical movement rejected: movement=%v err=%v", m, err)
	}
}

func TestExpensesPlus_CreateCashMovementIsIdempotent(t *testing.T) {
	f := setupExpenses(t)
	uc := expApp.NewCreateCashMovement(repos.NewCashMovementSQLiteRepository(), f.uow, f.recorder, expApp.OperationalPolicy{})
	in := expApp.CashMovementInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: mexicoBusinessDay(t),
		Amount: 100, MovementType: cashDomain.CashIn, Reason: "Fondo de cambio",
		IdempotencyKey: "cash-button-001",
	}
	first, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.ClassificationStatus != cashDomain.NonOperating {
		t.Fatalf("cash-in status=%q, want non_operating", first.ClassificationStatus)
	}
	second, err := uc.Execute(context.Background(), in)
	if err != nil || second.ID != first.ID {
		t.Fatalf("retry created another movement: first=%s second=%v err=%v", first.ID, second, err)
	}
	var count int
	if err = f.db.Get(&count, `SELECT COUNT(*) FROM cash_movements WHERE id=?`, first.ID); err != nil || count != 1 {
		t.Fatalf("stored rows=%d err=%v", count, err)
	}
	in.Amount = 101
	if _, err = uc.Execute(context.Background(), in); !errors.Is(err, expErrors.ErrIdempotencyKeyConflict) {
		t.Fatalf("reused key with different amount error=%v", err)
	}
}

func TestCashInCannotBeClassifiedAsExpenseAndNeverEntersPendingOutflows(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	day := mexicoBusinessDay(t)
	m, err := expApp.NewCreateCashMovement(moves, f.uow, f.recorder, expApp.OperationalPolicy{}).
		Execute(context.Background(), expApp.CashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day, Amount: 300,
			MovementType: cashDomain.CashIn, Reason: "Aporte de cambio", IdempotencyKey: "cash-in:not-expense",
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = expApp.NewClassifyCashMovement(moves, f.expenseRepo, f.uow, f.recorder, expApp.OperationalPolicy{}).
		Execute(context.Background(), expApp.ClassifyCashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementID: m.ID,
			Category: expenseDomain.CategoryOther, Classification: expenseDomain.ClassificationVariable,
		}); !errors.Is(err, expErrors.ErrCashInCannotBeClassified) {
		t.Fatalf("classify cash-in err=%v", err)
	}

	pending, err := expApp.NewListCashMovementsByDate(moves, f.uow).ExecuteList(context.Background(), expApp.ListCashMovementsInput{
		GymID: f.gymID, Status: "pending", Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	var expenses int
	if err = f.db.Get(&expenses, `SELECT COUNT(*) FROM expenses WHERE gym_id=? AND deleted_at IS NULL`, f.gymID); err != nil {
		t.Fatal(err)
	}
	var storedStatus string
	if err = f.db.Get(&storedStatus, `SELECT classification_status FROM cash_movements WHERE id=?`, m.ID); err != nil {
		t.Fatal(err)
	}
	if pending.Total != 0 || expenses != 0 || storedStatus != cashDomain.NonOperating {
		t.Fatalf("pending=%d expenses=%d status=%q; cash-in must stay outside outflows", pending.Total, expenses, storedStatus)
	}
}

func TestLegacyCashInExpense_CanBeRepairedAfterWriteBarrierWithoutChangingPhysicalMovement(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	day := mexicoBusinessDay(t)
	movement, err := expApp.NewCreateCashMovement(moves, f.uow, f.recorder, expApp.OperationalPolicy{}).
		Execute(context.Background(), expApp.CashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day, Amount: 275,
			MovementType: cashDomain.CashOut, Reason: "Aporte capturado como gasto",
			IdempotencyKey: "legacy:cash-in-expense",
		})
	if err != nil {
		t.Fatal(err)
	}
	linkedExpense, err := expApp.NewClassifyCashMovement(moves, f.expenseRepo, f.uow, f.recorder, expApp.OperationalPolicy{}).
		Execute(context.Background(), expApp.ClassifyCashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementID: movement.ID,
			Category: expenseDomain.CategoryOther, Classification: expenseDomain.ClassificationVariable,
		})
	if err != nil {
		t.Fatal(err)
	}

	// Reproduce a row written by the old bug before migration 041 installed the
	// forward barrier: the physical direction says cash-in but it owns Expense.
	if _, err = f.db.Exec(`UPDATE cash_movements SET movement_type='cash_in' WHERE id=?`, movement.ID); err != nil {
		t.Fatal(err)
	}
	applyCashInWriteBarrier(t, f)
	legacyRows, err := expApp.NewListCashMovementsByDate(moves, f.uow).ExecuteList(context.Background(), expApp.ListCashMovementsInput{
		GymID: f.gymID, Status: cashDomain.AsExpense, Page: 1, PageSize: 50,
	})
	if err != nil || legacyRows.Total != 1 || len(legacyRows.Items) != 1 || legacyRows.Items[0].ID != movement.ID {
		t.Fatalf("legacy expense movement must remain visible for repair: rows=%+v err=%v", legacyRows, err)
	}

	var version int
	if err = f.db.Get(&version, `SELECT version FROM cash_movements WHERE id=?`, movement.ID); err != nil {
		t.Fatal(err)
	}
	corrected, err := expApp.NewUnclassifyCashMovement(moves, f.uow, f.recorder).WithExpenses(f.expenseRepo).
		Execute(context.Background(), expApp.UnclassifyCashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", MovementID: movement.ID,
			ExpectedVersion: version, CorrectionReason: "era una aportación al cajón, no un gasto",
		})
	if err != nil {
		t.Fatal(err)
	}
	if corrected.MovementType != cashDomain.CashIn || corrected.ClassificationStatus != cashDomain.NonOperating ||
		corrected.ExpenseID != nil || corrected.DeletedAt != nil || corrected.Amount != 275 {
		t.Fatalf("corrected legacy movement=%+v", corrected)
	}
	var expenseTombstones, movementRows, expenseAudits, movementAudits int
	if err = f.db.Get(&expenseTombstones,
		`SELECT COUNT(*) FROM expenses WHERE id=? AND deleted_at IS NOT NULL AND cash_movement_id IS NULL`, linkedExpense.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&movementRows,
		`SELECT COUNT(*) FROM cash_movements WHERE id=? AND deleted_at IS NULL AND movement_type='cash_in' AND classification_status='non_operating' AND expense_id IS NULL AND amount=27500`, movement.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&expenseAudits,
		`SELECT COUNT(*) FROM audit_log WHERE entity_type='expenses' AND entity_id=? AND action='void_for_cash_reclassification'`, linkedExpense.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&movementAudits,
		`SELECT COUNT(*) FROM audit_log WHERE entity_type='cash_movements' AND entity_id=? AND action='unclassify_expense'`, movement.ID); err != nil {
		t.Fatal(err)
	}
	if expenseTombstones != 1 || movementRows != 1 || expenseAudits != 1 || movementAudits != 1 {
		t.Fatalf("expense_tombstones=%d movement_rows=%d expense_audits=%d movement_audits=%d",
			expenseTombstones, movementRows, expenseAudits, movementAudits)
	}
}

func TestLegacyCashInInventoryPurchase_CanBeReopenedAndTombstonedAfterWriteBarrier(t *testing.T) {
	f := setupExpenses(t)
	products := prodRepos.NewProductSQLiteRepository()
	stock := prodRepos.NewStockMovementSQLiteRepository()
	purchases := prodRepos.NewInventoryPurchaseSQLiteRepository()
	moves := repos.NewCashMovementSQLiteRepository()
	marker := &cashAdjustmentSpy{}
	cash := expApp.NewInventoryPurchaseCashService(moves).WithAudit(f.recorder).WithCashSessionMarker(marker)
	created, err := prodApp.NewCreateProduct(products, stock, f.uow, f.recorder).Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Toallas", Price: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	cost, isPurchase := 10.0, true
	restock, err := prodApp.NewAdjustStock(products, stock, f.uow, f.recorder).WithPurchases(purchases, cash).
		Execute(context.Background(), prodApp.AdjustStockInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ProductID: created.ProductID,
			MovementType: "restock", Quantity: 5, Cost: &cost, IsPurchase: &isPurchase,
			PurchaseStatus: purchaseDomain.StatusUnpaid, IdempotencyKey: "legacy:inventory:restock",
		})
	if err != nil || restock.PurchaseID == nil {
		t.Fatalf("restock=%+v err=%v", restock, err)
	}
	day := mexicoBusinessDay(t)
	paid, err := prodApp.NewPayInventoryPurchase(purchases, products, cash, f.uow, f.recorder).
		Execute(context.Background(), prodApp.PayInventoryPurchaseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PurchaseID: *restock.PurchaseID,
			ExpectedVersion: 1, PaidOn: day, PaymentMethod: purchaseDomain.MethodCash,
			PaidFrom: purchaseDomain.PaidFromCashDrawer, IdempotencyKey: "legacy:inventory:payment",
		})
	if err != nil || paid.Purchase.CashMovementID == nil {
		t.Fatalf("paid=%+v err=%v", paid, err)
	}
	movementID := *paid.Purchase.CashMovementID
	if _, err = f.db.Exec(`UPDATE cash_movements SET movement_type='cash_in' WHERE id=?`, movementID); err != nil {
		t.Fatal(err)
	}
	applyCashInWriteBarrier(t, f)

	reopened, err := prodApp.NewReopenInventoryPurchase(purchases, products, cash, f.uow, f.recorder).
		Execute(context.Background(), prodApp.ReopenInventoryPurchaseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PurchaseID: *restock.PurchaseID,
			ExpectedVersion: paid.Purchase.Version, CorrectionReason: "la salida se registró con la dirección incorrecta",
		})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Purchase.Status != purchaseDomain.StatusUnpaid || reopened.Purchase.CashMovementID != nil ||
		reopened.Purchase.TotalAmount == nil || *reopened.Purchase.TotalAmount != 50 || reopened.Purchase.Quantity != 5 {
		t.Fatalf("reopened purchase=%+v", reopened.Purchase)
	}
	var tombstones, livePurchases, audits int
	if err = f.db.Get(&tombstones,
		`SELECT COUNT(*) FROM cash_movements WHERE id=? AND deleted_at IS NOT NULL AND movement_type='cash_in' AND classification_status='inventory_purchase'`, movementID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&livePurchases,
		`SELECT COUNT(*) FROM inventory_purchases WHERE id=? AND deleted_at IS NULL AND status='unpaid' AND cash_movement_id IS NULL AND total_amount=5000`, *restock.PurchaseID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&audits,
		`SELECT COUNT(*) FROM audit_log WHERE entity_type='inventory_purchases' AND entity_id=? AND action='reopen_payment'`, *restock.PurchaseID); err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 || livePurchases != 1 || audits != 1 || marker.calls != 1 ||
		marker.input.GymID != f.gymID || marker.input.DrawerID != f.gymID || marker.input.OperationalDate.Format("2006-01-02") != day.Format("2006-01-02") {
		t.Fatalf("tombstones=%d live_purchase=%d audits=%d marker_calls=%d marker=%+v",
			tombstones, livePurchases, audits, marker.calls, marker.input)
	}
}

func TestCashMovementUpdateDelete_RequireReasonAndRejectInventoryPurchase(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	day := mexicoBusinessDay(t)
	created, err := expApp.NewCreateCashMovement(moves, f.uow, f.recorder, expApp.OperationalPolicy{}).Execute(context.Background(), expApp.CashMovementInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day,
		Amount: 200, MovementType: cashDomain.CashOut, Reason: "Compra de agua",
		IdempotencyKey: "cash:inventory:guard",
	})
	if err != nil {
		t.Fatal(err)
	}
	update := expApp.NewUpdateCashMovement(moves, f.uow, f.recorder, expApp.OperationalPolicy{})
	updateInput := expApp.CashMovementInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day,
		Amount: 210, MovementType: cashDomain.CashOut, Reason: "Compra de agua",
		ExpectedVersion: created.Version,
	}
	if _, err = update.Execute(context.Background(), created.ID, updateInput); !errors.Is(err, expErrors.ErrCorrectionReasonRequired) {
		t.Fatalf("update without reason err=%v", err)
	}
	deleteUC := expApp.NewDeleteCashMovement(moves, f.uow, f.recorder, expApp.OperationalPolicy{})
	if err = deleteUC.Execute(context.Background(), f.gymID, f.ownerID, created.ID, expApp.DeleteCashMovementOptions{ExpectedVersion: created.Version}); !errors.Is(err, expErrors.ErrCorrectionReasonRequired) {
		t.Fatalf("delete without reason err=%v", err)
	}
	if err = f.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		m, getErr := moves.GetByID(tx, f.gymID, created.ID)
		if getErr != nil {
			return getErr
		}
		v := m.Version
		if getErr = m.ClassifyInventoryPurchase(time.Now().UTC()); getErr != nil {
			return getErr
		}
		_, getErr = moves.Update(tx, m, v)
		return getErr
	}); err != nil {
		t.Fatal(err)
	}
	updateInput.CorrectionReason = "monto capturado incorrectamente"
	if _, err = update.Execute(context.Background(), created.ID, updateInput); !errors.Is(err, expErrors.ErrLinkedCashMovement) {
		t.Fatalf("inventory update err=%v", err)
	}
	if err = deleteUC.Execute(context.Background(), f.gymID, f.ownerID, created.ID, expApp.DeleteCashMovementOptions{
		ExpectedVersion: created.Version + 1, CorrectionReason: "compra capturada incorrectamente",
	}); !errors.Is(err, expErrors.ErrLinkedCashMovement) {
		t.Fatalf("inventory delete err=%v", err)
	}
}

func TestUnclassifyNonOperating_IsOwnerOnlyAndAudited(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	day := mexicoBusinessDay(t)
	m, err := expApp.NewCreateCashMovement(moves, f.uow, f.recorder, expApp.OperationalPolicy{}).Execute(context.Background(), expApp.CashMovementInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day, Amount: 50,
		MovementType: cashDomain.CashOut, Reason: "Retiro personal", IdempotencyKey: "cash:non-operating:undo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = expApp.NewClassifyCashMovement(moves, f.expenseRepo, f.uow, f.recorder, expApp.OperationalPolicy{}).Execute(context.Background(), expApp.ClassifyCashMovementInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MovementID: m.ID, AsNonOperating: true,
	}); err != nil {
		t.Fatal(err)
	}
	var classifiedVersion int
	if err = f.db.Get(&classifiedVersion, `SELECT version FROM cash_movements WHERE id=?`, m.ID); err != nil {
		t.Fatal(err)
	}
	undo := expApp.NewUnclassifyCashMovement(moves, f.uow, f.recorder)
	in := expApp.UnclassifyCashMovementInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "operator", MovementID: m.ID,
		ExpectedVersion: classifiedVersion, CorrectionReason: "se clasificó por error",
	}
	if _, err = undo.Execute(context.Background(), in); !errors.Is(err, expErrors.ErrCorrectionOwnerRequired) {
		t.Fatalf("operator undo err=%v", err)
	}
	in.ActorRole = "owner"
	corrected, err := undo.Execute(context.Background(), in)
	if err != nil || corrected.ClassificationStatus != cashDomain.Unclassified {
		t.Fatalf("corrected=%+v err=%v", corrected, err)
	}
	var audits int
	if err = f.db.Get(&audits, `SELECT COUNT(*) FROM audit_log WHERE entity_type='cash_movements' AND entity_id=? AND action='unclassify_non_operating'`, m.ID); err != nil || audits != 1 {
		t.Fatalf("audit rows=%d err=%v", audits, err)
	}
}

func TestUnclassifyExpense_VoidsOnlyFinancialClassificationAndCanReclassify(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	day := mexicoBusinessDay(t)
	m, err := expApp.NewCreateCashMovement(moves, f.uow, f.recorder, expApp.OperationalPolicy{}).Execute(context.Background(), expApp.CashMovementInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day, Amount: 275,
		MovementType: cashDomain.CashOut, Reason: "Pago capturado en recepción", IdempotencyKey: "cash:expense:undo",
	})
	if err != nil {
		t.Fatal(err)
	}
	classify := expApp.NewClassifyCashMovement(moves, f.expenseRepo, f.uow, f.recorder, expApp.OperationalPolicy{})
	firstExpense, err := classify.Execute(context.Background(), expApp.ClassifyCashMovementInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MovementID: m.ID,
		Category: expenseDomain.CategoryMaintenance, Classification: expenseDomain.ClassificationVariable,
	})
	if err != nil {
		t.Fatal(err)
	}
	tx := mustQuery(t, f.uow)
	classified, err := moves.GetByID(tx, f.gymID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := expApp.NewUnclassifyCashMovement(moves, f.uow, f.recorder).WithExpenses(f.expenseRepo).
		Execute(context.Background(), expApp.UnclassifyCashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", MovementID: m.ID,
			ExpectedVersion: classified.Version, CorrectionReason: "se eligió la categoría equivocada",
		})
	if err != nil {
		t.Fatal(err)
	}
	if corrected.ClassificationStatus != cashDomain.Unclassified || corrected.ExpenseID != nil ||
		corrected.Amount != 275 || corrected.MovementOn.Format("2006-01-02") != day.Format("2006-01-02") {
		t.Fatalf("physical movement changed while unclassifying: %+v", corrected)
	}
	var tombstones, physicalRows, expenseAudits, movementAudits int
	if err = f.db.Get(&tombstones, `SELECT COUNT(*) FROM expenses WHERE id=? AND deleted_at IS NOT NULL AND cash_movement_id IS NULL`, firstExpense.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&physicalRows, `SELECT COUNT(*) FROM cash_movements WHERE id=? AND deleted_at IS NULL AND amount=27500`, m.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&expenseAudits, `SELECT COUNT(*) FROM audit_log WHERE entity_type='expenses' AND entity_id=? AND action='void_for_cash_reclassification'`, firstExpense.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&movementAudits, `SELECT COUNT(*) FROM audit_log WHERE entity_type='cash_movements' AND entity_id=? AND action='unclassify_expense'`, m.ID); err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 || physicalRows != 1 || expenseAudits != 1 || movementAudits != 1 {
		t.Fatalf("tombstones=%d physical=%d expense_audits=%d movement_audits=%d", tombstones, physicalRows, expenseAudits, movementAudits)
	}
	secondExpense, err := classify.Execute(context.Background(), expApp.ClassifyCashMovementInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MovementID: m.ID,
		Category: expenseDomain.CategoryOther, Classification: expenseDomain.ClassificationVariable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondExpense.ID == firstExpense.ID {
		t.Fatalf("reclassification reused expense tombstone %s", firstExpense.ID)
	}
	var allExpenses, liveExpenses int
	if err = f.db.Get(&allExpenses, `SELECT COUNT(*) FROM expenses WHERE gym_id=?`, f.gymID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&liveExpenses, `SELECT COUNT(*) FROM expenses WHERE gym_id=? AND deleted_at IS NULL`, f.gymID); err != nil {
		t.Fatal(err)
	}
	if allExpenses != 2 || liveExpenses != 1 {
		t.Fatalf("all expenses=%d live=%d, want immutable history 2/1", allExpenses, liveExpenses)
	}
}

func TestCashMovementHistory_IsPaginatedAndStatusHasProductSemantics(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	create := expApp.NewCreateCashMovement(moves, f.uow, f.recorder, expApp.OperationalPolicy{})
	day := mexicoBusinessDay(t)
	inputs := []expApp.CashMovementInput{
		{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day, Amount: 500, MovementType: cashDomain.CashIn, Reason: "Fondo inicial", IdempotencyKey: "history:in"},
		{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day, Amount: 80, MovementType: cashDomain.CashOut, Reason: "Compra por clasificar", IdempotencyKey: "history:pending"},
		{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day, Amount: 40, MovementType: cashDomain.CashOut, Reason: "Retiro personal", IdempotencyKey: "history:classified"},
	}
	rows := make([]*cashDomain.CashMovement, 0, len(inputs))
	for _, in := range inputs {
		m, err := create.Execute(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, m)
	}
	if _, err := expApp.NewClassifyCashMovement(moves, f.expenseRepo, f.uow, f.recorder, expApp.OperationalPolicy{}).
		Execute(context.Background(), expApp.ClassifyCashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementID: rows[2].ID, AsNonOperating: true,
		}); err != nil {
		t.Fatal(err)
	}
	list := expApp.NewListCashMovementsByDate(moves, f.uow)
	all, err := list.ExecuteList(context.Background(), expApp.ListCashMovementsInput{
		GymID: f.gymID, From: &day, To: &day, Status: "all", Page: 1, PageSize: 2,
	})
	if err != nil || all.Total != 3 || len(all.Items) != 2 || all.PageSize != 2 {
		t.Fatalf("all=%+v err=%v", all, err)
	}
	pending, err := list.ExecuteList(context.Background(), expApp.ListCashMovementsInput{GymID: f.gymID, Status: "pending", Page: 1, PageSize: 50})
	if err != nil || pending.Total != 1 || len(pending.Items) != 1 || pending.Items[0].ID != rows[1].ID {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	classified, err := list.ExecuteList(context.Background(), expApp.ListCashMovementsInput{GymID: f.gymID, Status: "classified", Page: 1, PageSize: 50})
	if err != nil || classified.Total != 2 || len(classified.Items) != 2 {
		t.Fatalf("classified=%+v err=%v", classified, err)
	}
	nonOperating, err := list.ExecuteList(context.Background(), expApp.ListCashMovementsInput{GymID: f.gymID, Status: cashDomain.NonOperating, Page: 1, PageSize: 50})
	if err != nil || nonOperating.Total != 2 || len(nonOperating.Items) != 2 {
		t.Fatalf("non-operating=%+v err=%v; both physical directions must be visible", nonOperating, err)
	}
}

func TestReopenPaidOccurrence_TombstonesLinkedMoneyAndCanBePaidAgain(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	moves := repos.NewCashMovementSQLiteRepository()
	day := mexicoBusinessDay(t)
	if _, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(context.Background(), expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Limpieza", Category: expenseDomain.CategoryMaintenance,
		ExpectedAmount: 600, PaymentMethod: expenseDomain.PaymentCash,
		Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: day,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(context.Background(), f.gymID, day); err != nil {
		t.Fatal(err)
	}
	rows, err := expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("occurrences=%d err=%v", len(rows), err)
	}
	pay := expApp.NewMarkExpenseOccurrencePaid(occurrences, f.expenseRepo, moves, f.uow, f.recorder, expApp.OperationalPolicy{})
	first, err := pay.Execute(context.Background(), expApp.MarkOccurrencePaidInput{
		GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID, PaidOn: day,
		Amount: 600, PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister,
		IdempotencyKey: "occurrence:first-payment",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := occurrences.GetByID(mustQuery(t, f.uow), f.gymID, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := expApp.NewReopenExpenseOccurrence(occurrences, f.expenseRepo, moves, f.uow, f.recorder).Execute(context.Background(), expApp.ReopenExpenseOccurrenceInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", OccurrenceID: rows[0].ID,
		ExpectedVersion: resolved.Version, CorrectionReason: "se registró el pago antes de recibirlo",
	})
	if err != nil || reopened.Status != recurring.Pending || reopened.ExpenseID != nil {
		t.Fatalf("reopened=%+v err=%v", reopened, err)
	}
	var liveExpenses, liveMovements int
	if err = f.db.Get(&liveExpenses, `SELECT COUNT(*) FROM expenses WHERE id=? AND deleted_at IS NULL`, first.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&liveMovements, `SELECT COUNT(*) FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, first.ID); err != nil {
		t.Fatal(err)
	}
	if liveExpenses != 0 || liveMovements != 0 {
		t.Fatalf("old live expenses=%d movements=%d", liveExpenses, liveMovements)
	}
	second, err := pay.Execute(context.Background(), expApp.MarkOccurrencePaidInput{
		GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID, PaidOn: day,
		Amount: 620, PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister,
		IdempotencyKey: "occurrence:corrected-payment",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("corrected payment reused tombstone id %s", first.ID)
	}
	if err = f.db.Get(&liveExpenses, `SELECT COUNT(*) FROM expenses WHERE recurring_occurrence_id=? AND deleted_at IS NULL`, rows[0].ID); err != nil || liveExpenses != 1 {
		t.Fatalf("new live expense count=%d err=%v", liveExpenses, err)
	}
}

func mustQuery(t *testing.T, uow sharedDomain.UnitOfWork) sharedDomain.Transaction {
	t.Helper()
	tx, err := uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestExpensesPlus_DeleteQueuesDurableTombstone(t *testing.T) {
	f := setupExpenses(t)
	day := mexicoBusinessDay(t)
	out, err := f.createUC().Execute(context.Background(), expApp.CreateExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day, Amount: 100, Category: expenseDomain.CategoryOther, PaymentMethod: expenseDomain.PaymentTransfer, PaidFrom: expenseDomain.PaidFromGymFund})
	if err != nil {
		t.Fatal(err)
	}
	del := expApp.NewDeleteExpense(f.expenseRepo, f.uow, f.recorder)
	if err = del.Execute(context.Background(), expApp.DeleteExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: out.ExpenseID, CorrectionReason: "el gasto se capturó por error"}); err != nil {
		t.Fatal(err)
	}
	var op, payload string
	if err = f.db.QueryRow(`SELECT operation,payload FROM sync_queue WHERE entity_type='expenses' AND entity_id=? AND synced_at IS NULL`, out.ExpenseID).Scan(&op, &payload); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if json.Unmarshal([]byte(payload), &data) != nil {
		t.Fatal("bad payload")
	}
	if op != "delete" || data["deleted_at"] == nil {
		t.Fatalf("operation=%s deleted_at=%v", op, data["deleted_at"])
	}
}

func TestExpensesPlus_ManualCashExpenseHasOneCoherentDrawerMovement(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	policy := expApp.OperationalPolicy{DayLock: repos.NewCashDayLockSQLite()}
	day := mexicoBusinessDay(t)
	description := "Compra de limpieza"
	created, err := expApp.NewCreateExpense(f.expenseRepo, f.uow, f.recorder).WithOperational(moves, policy).Execute(context.Background(), expApp.CreateExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day, Amount: 350, Category: expenseDomain.CategoryNonInventorySupplies, Description: &description, PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister, Classification: expenseDomain.ClassificationVariable})
	if err != nil {
		t.Fatal(err)
	}
	var live int
	if err = f.db.Get(&live, `SELECT COUNT(*) FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, created.ExpenseID); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("live linked movements=%d, want 1", live)
	}
	// El mismo método efectivo puede salir del fondo acumulado sin tocar la
	// caja del día: PaidFrom, no PaymentMethod, gobierna el movimiento.
	if err = expApp.NewUpdateExpense(f.expenseRepo, f.uow, f.recorder).WithOperational(moves, policy).Execute(context.Background(), expApp.UpdateExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: created.ExpenseID, ExpenseDate: day, Amount: 350, Category: expenseDomain.CategoryNonInventorySupplies, Description: &description, PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromGymFund, Classification: expenseDomain.ClassificationVariable, ExpectedVersion: 1, CorrectionReason: "el pago salió del fondo acumulado"}); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&live, `SELECT COUNT(*) FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, created.ExpenseID); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("live linked movements after moving payment to gym fund=%d, want 0", live)
	}
}

func TestExpensesPlus_UpdateRepairsOneSidedExpenseCashMovementLink(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	policy := expApp.OperationalPolicy{DayLock: repos.NewCashDayLockSQLite()}
	day := mexicoBusinessDay(t)
	description := "Pago de CFE"
	created, err := expApp.NewCreateExpense(f.expenseRepo, f.uow, f.recorder).
		WithOperational(moves, policy).
		Execute(context.Background(), expApp.CreateExpenseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day,
			Amount: 2300, Category: expenseDomain.CategoryUtilities,
			Description: &description, PaymentMethod: expenseDomain.PaymentCash,
			PaidFrom:       expenseDomain.PaidFromCashRegister,
			Classification: expenseDomain.ClassificationFixed,
		})
	if err != nil {
		t.Fatal(err)
	}
	var originalMovementID string
	if err = f.db.Get(&originalMovementID, `SELECT id FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, created.ExpenseID); err != nil {
		t.Fatal(err)
	}
	// Reproduce the inconsistent row observed after sync: the movement still
	// owns the expense, but the expense no longer has its forward link.
	if _, err = f.db.Exec(`UPDATE expenses SET cash_movement_id=NULL WHERE id=?`, created.ExpenseID); err != nil {
		t.Fatal(err)
	}

	if err = expApp.NewUpdateExpense(f.expenseRepo, f.uow, f.recorder).
		WithOperational(moves, policy).
		Execute(context.Background(), expApp.UpdateExpenseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: created.ExpenseID,
			ExpenseDate: day, Amount: 2350, Category: expenseDomain.CategoryUtilities,
			Description: &description, PaymentMethod: expenseDomain.PaymentCash,
			PaidFrom:       expenseDomain.PaidFromCashRegister,
			Classification: expenseDomain.ClassificationFixed, ExpectedVersion: 1,
			CorrectionReason: "el monto pagado fue distinto al capturado",
		}); err != nil {
		t.Fatal(err)
	}

	var live int
	if err = f.db.Get(&live, `SELECT COUNT(*) FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, created.ExpenseID); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("live linked movements=%d, want 1", live)
	}
	var linkedMovementID string
	var movementAmount int64
	if err = f.db.QueryRow(`SELECT e.cash_movement_id,m.amount FROM expenses e JOIN cash_movements m ON m.id=e.cash_movement_id WHERE e.id=?`, created.ExpenseID).Scan(&linkedMovementID, &movementAmount); err != nil {
		t.Fatal(err)
	}
	if linkedMovementID != originalMovementID {
		t.Fatalf("linked movement=%s, want original %s", linkedMovementID, originalMovementID)
	}
	if movementAmount != 235000 {
		t.Fatalf("movement amount cents=%d, want 235000", movementAmount)
	}
}

func TestExpensesPlus_DeleteRepairsOneSidedExpenseCashMovementLink(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	policy := expApp.OperationalPolicy{DayLock: repos.NewCashDayLockSQLite()}
	day := mexicoBusinessDay(t)
	created, err := expApp.NewCreateExpense(f.expenseRepo, f.uow, f.recorder).
		WithOperational(moves, policy).
		Execute(context.Background(), expApp.CreateExpenseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day,
			Amount: 100, Category: expenseDomain.CategoryOther,
			PaymentMethod:  expenseDomain.PaymentCash,
			PaidFrom:       expenseDomain.PaidFromCashRegister,
			Classification: expenseDomain.ClassificationVariable,
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`UPDATE expenses SET cash_movement_id=NULL WHERE id=?`, created.ExpenseID); err != nil {
		t.Fatal(err)
	}
	if err = expApp.NewDeleteExpense(f.expenseRepo, f.uow, f.recorder).
		WithOperational(moves, policy).
		Execute(context.Background(), expApp.DeleteExpenseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: created.ExpenseID,
			ExpectedVersion: 1, CorrectionReason: "el gasto se registró por error",
		}); err != nil {
		t.Fatal(err)
	}
	var live int
	if err = f.db.Get(&live, `SELECT COUNT(*) FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, created.ExpenseID); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("live linked movements after delete=%d, want 0", live)
	}
}

func TestExpensesPlus_OwnerRepairsLegacyCashSourceWithoutInventingMovement(t *testing.T) {
	f := setupExpenses(t)
	moves := repos.NewCashMovementSQLiteRepository()
	policy := expApp.OperationalPolicy{DayLock: repos.NewCashDayLockSQLite()}
	day := mexicoBusinessDay(t)
	now := time.Now().UTC().UnixMilli()
	seedLegacy := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := f.db.Exec(`INSERT INTO expenses
			(id,gym_id,version,created_at,updated_at,expense_date,amount,category,payment_method,paid_from,created_by)
			VALUES(?,?,1,?,?,?,5000,'otros','cash','cash_register',?)`,
			id, f.gymID, now, now, day.Format("2006-01-02"), f.ownerID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	update := expApp.NewUpdateExpense(f.expenseRepo, f.uow, f.recorder).WithOperational(moves, policy)

	// Confirming Fondo keeps the economic expense and creates no drawer row.
	fundExpenseID := seedLegacy()
	if err := update.Execute(context.Background(), expApp.UpdateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: fundExpenseID,
		ExpenseDate: day, Amount: 50, Category: expenseDomain.CategoryOther,
		PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromGymFund,
		Classification: expenseDomain.ClassificationVariable, ExpectedVersion: 1,
		CorrectionReason: "confirmado como pago desde el fondo",
	}); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := f.db.Get(&live, `SELECT COUNT(*) FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, fundExpenseID); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("fund repair created %d drawer movements, want 0", live)
	}

	// Confirming Caja creates exactly one deterministic linked movement.
	drawerExpenseID := seedLegacy()
	if err := update.Execute(context.Background(), expApp.UpdateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: drawerExpenseID,
		ExpenseDate: day, Amount: 50, Category: expenseDomain.CategoryOther,
		PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashDrawer,
		Classification: expenseDomain.ClassificationVariable, ExpectedVersion: 1,
		CorrectionReason: "confirmado como salida de la caja del día",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Get(&live, `SELECT COUNT(*) FROM cash_movements WHERE expense_id=? AND deleted_at IS NULL`, drawerExpenseID); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("drawer repair created %d live movements, want exactly 1", live)
	}
	var linkedID string
	if err := f.db.Get(&linkedID, `SELECT cash_movement_id FROM expenses WHERE id=?`, drawerExpenseID); err != nil {
		t.Fatal(err)
	}
	if linkedID == "" {
		t.Fatal("drawer repair must link the expense to its physical movement")
	}
}

func TestExpensesPlus_CrossGymExpenseAccessIsNotFound(t *testing.T) {
	f := setupExpenses(t)
	day := mexicoBusinessDay(t)
	created, err := f.createUC().Execute(context.Background(), expApp.CreateExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day, Amount: 99, Category: expenseDomain.CategoryOther, PaymentMethod: expenseDomain.PaymentTransfer, PaidFrom: expenseDomain.PaidFromGymFund})
	if err != nil {
		t.Fatal(err)
	}
	err = expApp.NewDeleteExpense(f.expenseRepo, f.uow, f.recorder).Execute(context.Background(), expApp.DeleteExpenseInput{GymID: uuid.New(), ActorUserID: f.ownerID, ExpenseID: created.ExpenseID})
	if !errors.Is(err, expErrors.ErrExpenseNotFound) {
		t.Fatalf("cross-gym delete error=%v, want not found", err)
	}
}

func TestExpensesPlus_TemplateEditChangesOnlyFuturePendingSnapshots(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	moves := repos.NewCashMovementSQLiteRepository()
	day := mexicoBusinessDay(t)

	tpl, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(context.Background(), expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Servicio", Category: expenseDomain.CategoryUtilities,
		ExpectedAmount: 100, PaymentMethod: expenseDomain.PaymentTransfer,
		Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: day,
	})
	if err != nil {
		t.Fatal(err)
	}
	to := day.AddDate(0, 1, 0)
	if _, err = expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(context.Background(), f.gymID, to); err != nil {
		t.Fatal(err)
	}
	rows, err := expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: to, Limit: 10})
	if err != nil || len(rows) != 2 {
		t.Fatalf("occurrences=%d err=%v", len(rows), err)
	}
	if _, err = expApp.NewMarkExpenseOccurrencePaid(occurrences, f.expenseRepo, moves, f.uow, f.recorder, expApp.OperationalPolicy{DayLock: repos.NewCashDayLockSQLite()}).Execute(context.Background(), expApp.MarkOccurrencePaidInput{
		GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID,
		PaidOn: day, Amount: 100, PaymentMethod: expenseDomain.PaymentTransfer,
		PaidFrom: expenseDomain.PaidFromGymFund,
	}); err != nil {
		t.Fatal(err)
	}

	currentTemplates, err := expApp.NewListRecurringExpenseTemplates(templates, f.uow).Execute(context.Background(), f.gymID, true)
	if err != nil || len(currentTemplates) != 1 {
		t.Fatalf("current template: len=%d err=%v", len(currentTemplates), err)
	}
	currentVersion := currentTemplates[0].Version
	updated, err := expApp.NewUpdateRecurringExpenseTemplate(templates, occurrences, f.uow, f.recorder, expApp.OperationalPolicy{}).Execute(context.Background(), tpl.ID, expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Servicio actualizado", Category: expenseDomain.CategoryMarketing,
		ExpectedAmount: 225, PaymentMethod: expenseDomain.PaymentCard,
		Classification: expenseDomain.ClassificationVariable, Frequency: recurring.Monthly,
		StartsOn: day, ExpectedVersion: currentVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != currentVersion+1 {
		t.Fatalf("updated version=%d", updated.Version)
	}
	rows, err = expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: to, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var resolved, future *recurring.Occurrence
	for _, row := range rows {
		if row.Status == recurring.Paid {
			resolved = row
		} else if row.Status == recurring.Pending {
			future = row
		}
	}
	if resolved == nil || resolved.ExpectedAmount != 100 || resolved.Classification != expenseDomain.ClassificationFixed {
		t.Fatalf("resolved historical snapshot changed: %+v", resolved)
	}
	if future == nil || future.ExpectedAmount != 225 || future.Category != expenseDomain.CategoryMarketing || future.PaymentMethod != expenseDomain.PaymentCard || future.Classification != expenseDomain.ClassificationVariable {
		t.Fatalf("future pending snapshot not updated: %+v", future)
	}
}

func TestExpensesPlus_RecurringTemplateCreateIsIdempotent(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	create := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder)
	day := mexicoBusinessDay(t)
	in := expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Renta", Category: expenseDomain.CategoryRent,
		ExpectedAmount: 8000, PaymentMethod: expenseDomain.PaymentTransfer,
		Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: day,
		IdempotencyKey: "template:rent:2026",
	}
	first, err := create.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := create.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if second.ID != first.ID || second.Version != first.Version {
		t.Fatalf("exact retry first=%+v second=%+v", first, second)
	}
	rows, err := expApp.NewListRecurringExpenseTemplates(templates, f.uow).Execute(context.Background(), f.gymID, true)
	if err != nil || len(rows) != 1 {
		t.Fatalf("templates=%d err=%v, want one", len(rows), err)
	}
	in.ExpectedAmount = 8100
	if _, err = create.Execute(context.Background(), in); !errors.Is(err, expErrors.ErrTemplateIdempotencyConflict) {
		t.Fatalf("changed retry err=%v, want idempotency conflict", err)
	}
}

func TestExpensesPlus_TemplateScheduleChangeRequiresResolvingPendingOccurrences(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	day := mexicoBusinessDay(t)
	tpl, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(context.Background(), expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Renta", Category: expenseDomain.CategoryRent,
		ExpectedAmount: 8000, PaymentMethod: expenseDomain.PaymentTransfer,
		Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: day,
		IdempotencyKey: "template:schedule-change",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(context.Background(), f.gymID, day); err != nil {
		t.Fatal(err)
	}
	current, err := expApp.NewListRecurringExpenseTemplates(templates, f.uow).Execute(context.Background(), f.gymID, true)
	if err != nil || len(current) != 1 {
		t.Fatalf("template list len=%d err=%v", len(current), err)
	}
	newStart := day.AddDate(0, 0, 1)
	update := expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: tpl.Name, Category: tpl.Category,
		ExpectedAmount: tpl.ExpectedAmount, PaymentMethod: tpl.PaymentMethod,
		Classification: tpl.Classification, Frequency: recurring.Monthly, StartsOn: newStart,
		ExpectedVersion: current[0].Version,
	}
	updateUC := expApp.NewUpdateRecurringExpenseTemplate(templates, occurrences, f.uow, f.recorder, expApp.OperationalPolicy{})
	if _, err = updateUC.Execute(context.Background(), tpl.ID, update); !errors.Is(err, expErrors.ErrTemplateScheduleHasPending) {
		t.Fatalf("schedule edit err=%v, want pending-occurrence conflict", err)
	}
	rows, err := expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("occurrences=%d err=%v", len(rows), err)
	}
	if err = expApp.NewSkipExpenseOccurrence(occurrences, f.uow, f.recorder).Execute(context.Background(), f.gymID, f.ownerID, rows[0].ID, "el contrato cambió de fecha"); err != nil {
		t.Fatal(err)
	}
	updated, err := updateUC.Execute(context.Background(), tpl.ID, update)
	if err != nil {
		t.Fatalf("schedule edit after resolving pending: %v", err)
	}
	if !updated.NextDueOn.Equal(newStart) {
		t.Fatalf("next due=%s want=%s", updated.NextDueOn.Format("2006-01-02"), newStart.Format("2006-01-02"))
	}
	rows, err = expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Status != recurring.Skipped || !rows[0].DueOn.Equal(day) {
		t.Fatalf("resolved history was rewritten: rows=%+v err=%v", rows, err)
	}
}

func TestExpensesPlus_PauseStopsGenerationButKeepsOccurrencesPayable(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	movements := repos.NewCashMovementSQLiteRepository()
	day := mexicoBusinessDay(t)
	tpl, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(context.Background(), expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Internet", Category: expenseDomain.CategoryUtilities,
		ExpectedAmount: 900, PaymentMethod: expenseDomain.PaymentTransfer,
		Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: day,
		IdempotencyKey: "template:pause-keeps-pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	materialize := expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder)
	if n, materializeErr := materialize.Execute(context.Background(), f.gymID, day); materializeErr != nil || n != 1 {
		t.Fatalf("initial materialize n=%d err=%v", n, materializeErr)
	}
	current, err := expApp.NewListRecurringExpenseTemplates(templates, f.uow).Execute(context.Background(), f.gymID, true)
	if err != nil || len(current) != 1 {
		t.Fatalf("template list len=%d err=%v", len(current), err)
	}
	activeUC := expApp.NewDeactivateRecurringExpenseTemplate(templates, f.uow, f.recorder).WithOperationalPolicy(expApp.OperationalPolicy{})
	paused, err := activeUC.Execute(context.Background(), f.gymID, f.ownerID, tpl.ID, false, current[0].Version)
	if err != nil || paused.Active {
		t.Fatalf("paused=%+v err=%v", paused, err)
	}
	if n, materializeErr := materialize.Execute(context.Background(), f.gymID, day.AddDate(0, 2, 0)); materializeErr != nil || n != 0 {
		t.Fatalf("paused materialize n=%d err=%v", n, materializeErr)
	}
	rows, err := expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day.AddDate(0, 2, 0), Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Status != recurring.Pending {
		t.Fatalf("pending occurrence disappeared: rows=%+v err=%v", rows, err)
	}
	paid, err := expApp.NewMarkExpenseOccurrencePaid(occurrences, f.expenseRepo, movements, f.uow, f.recorder, expApp.OperationalPolicy{}).Execute(context.Background(), expApp.MarkOccurrencePaidInput{
		GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID,
		PaidOn: day, Amount: 900, PaymentMethod: expenseDomain.PaymentTransfer,
		PaidFrom:       expenseDomain.PaidFromGymFund,
		IdempotencyKey: "occurrence:internet:paid-while-paused",
	})
	if err != nil || paid == nil {
		t.Fatalf("pay while paused expense=%+v err=%v", paid, err)
	}
	resumed, err := activeUC.Execute(context.Background(), f.gymID, f.ownerID, tpl.ID, true, paused.Version)
	if err != nil || !resumed.Active {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
	nextMonth := day.AddDate(0, 1, 0)
	if n, materializeErr := materialize.Execute(context.Background(), f.gymID, nextMonth); materializeErr != nil || n != 1 {
		t.Fatalf("resumed materialize n=%d err=%v", n, materializeErr)
	}
}

func TestExpensesPlus_ReactivateSkipsUngeneratedDatesElapsedWhilePaused(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	today := mexicoBusinessDay(t)
	starts := today.AddDate(0, -3, 0)
	tpl, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(context.Background(), expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Seguro", Category: expenseDomain.CategoryOther,
		ExpectedAmount: 500, PaymentMethod: expenseDomain.PaymentTransfer,
		Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: starts,
		IdempotencyKey: "template:resume-without-backfill",
	})
	if err != nil {
		t.Fatal(err)
	}
	activeUC := expApp.NewDeactivateRecurringExpenseTemplate(templates, f.uow, f.recorder).WithOperationalPolicy(expApp.OperationalPolicy{})
	paused, err := activeUC.Execute(context.Background(), f.gymID, f.ownerID, tpl.ID, false, tpl.Version)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := activeUC.Execute(context.Background(), f.gymID, f.ownerID, tpl.ID, true, paused.Version)
	if err != nil {
		t.Fatal(err)
	}
	wantDue, err := recurring.FirstDueOnOrAfter(starts, recurring.Monthly, today)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.NextDueOn.Equal(wantDue) || resumed.NextDueOn.Before(today) {
		t.Fatalf("resume next due=%s want=%s", resumed.NextDueOn.Format("2006-01-02"), wantDue.Format("2006-01-02"))
	}
	if n, materializeErr := expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(context.Background(), f.gymID, today); materializeErr != nil || n != 1 {
		t.Fatalf("materialize after resume n=%d err=%v", n, materializeErr)
	}
	rows, err := expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(context.Background(), expRepo.OccurrenceQuery{GymID: f.gymID, From: starts, To: today, Limit: 20})
	if err != nil || len(rows) != 1 || !rows[0].DueOn.Equal(wantDue) {
		t.Fatalf("backfilled paused dates: rows=%+v err=%v", rows, err)
	}
}

func TestExpensesPlus_OccurrenceHistoryIsPaginatedWithoutSilentTruncation(t *testing.T) {
	f := setupExpenses(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	day := mexicoBusinessDay(t)
	if _, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(context.Background(), expApp.RecurringTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Renta", Category: expenseDomain.CategoryRent,
		ExpectedAmount: 8000, PaymentMethod: expenseDomain.PaymentTransfer,
		Classification: expenseDomain.ClassificationFixed, Frequency: recurring.Monthly, StartsOn: day,
		IdempotencyKey: "template:paginated-occurrences",
	}); err != nil {
		t.Fatal(err)
	}
	to := day.AddDate(0, 3, 0)
	if n, err := expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(context.Background(), f.gymID, to); err != nil || n != 4 {
		t.Fatalf("materialize n=%d err=%v", n, err)
	}
	page, err := expApp.NewListExpenseOccurrences(occurrences, f.uow).ExecutePage(context.Background(), expRepo.OccurrenceQuery{
		GymID: f.gymID, From: day, To: to, Status: recurring.Pending,
	}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 4 || page.Page != 2 || page.PageSize != 2 || len(page.Items) != 2 {
		t.Fatalf("page=%+v, want total=4 page=2 page_size=2 items=2", page)
	}
	if !page.Items[0].DueOn.After(day) {
		t.Fatalf("second page started at %s", page.Items[0].DueOn.Format("2006-01-02"))
	}
	if _, err = expApp.NewListExpenseOccurrences(occurrences, f.uow).ExecutePage(context.Background(), expRepo.OccurrenceQuery{
		GymID: f.gymID, From: day, To: to, Status: "unknown",
	}, 1, 50); !errors.Is(err, expErrors.ErrInvalidOccurrenceStatus) {
		t.Fatalf("invalid status err=%v", err)
	}
}

func TestExpensesPlus_OptimisticVersionConflictDoesNotMutateExpense(t *testing.T) {
	f := setupExpenses(t)
	day := mexicoBusinessDay(t)
	created, err := f.createUC().Execute(context.Background(), expApp.CreateExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day, Amount: 100, Category: expenseDomain.CategoryOther, PaymentMethod: expenseDomain.PaymentTransfer, PaidFrom: expenseDomain.PaidFromGymFund})
	if err != nil {
		t.Fatal(err)
	}
	err = expApp.NewUpdateExpense(f.expenseRepo, f.uow, f.recorder).Execute(context.Background(), expApp.UpdateExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: created.ExpenseID, ExpenseDate: day, Amount: 999, Category: expenseDomain.CategoryMarketing, PaymentMethod: expenseDomain.PaymentTransfer, PaidFrom: expenseDomain.PaidFromGymFund, Classification: expenseDomain.ClassificationVariable, ExpectedVersion: 99})
	if !errors.Is(err, expErrors.ErrVersionConflict) {
		t.Fatalf("error=%v, want version conflict", err)
	}
	got, err := expApp.NewGetExpense(f.expenseRepo, f.uow).Execute(context.Background(), f.gymID, created.ExpenseID)
	if err != nil || got.Amount != 100 || got.Version != 1 {
		t.Fatalf("expense mutated after conflict: %+v err=%v", got, err)
	}
}

func TestExpensesPlus_AuditFailureRollsBackExpenseCashAndSync(t *testing.T) {
	f := setupExpenses(t)
	day := mexicoBusinessDay(t)
	_, err := expApp.NewCreateExpense(f.expenseRepo, f.uow, failingExpenseAudit{}).
		WithOperational(repos.NewCashMovementSQLiteRepository(), expApp.OperationalPolicy{DayLock: repos.NewCashDayLockSQLite()}).
		Execute(context.Background(), expApp.CreateExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day, Amount: 430, Category: expenseDomain.CategoryMaintenance, PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister})
	if err == nil {
		t.Fatal("audit failure was ignored")
	}
	for table, entityType := range map[string]string{"expenses": "expenses", "cash_movements": "cash_movements"} {
		var rows, queued int
		if dbErr := f.db.Get(&rows, "SELECT COUNT(*) FROM "+table); dbErr != nil {
			t.Fatal(dbErr)
		}
		if dbErr := f.db.Get(&queued, `SELECT COUNT(*) FROM sync_queue WHERE entity_type=?`, entityType); dbErr != nil {
			t.Fatal(dbErr)
		}
		if rows != 0 || queued != 0 {
			t.Fatalf("%s rows=%d queued=%d after audit rollback", table, rows, queued)
		}
	}
}
