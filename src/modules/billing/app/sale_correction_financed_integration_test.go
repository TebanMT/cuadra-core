//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	cashcloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
	billingRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func TestCorrectSale_IdempotencyReplaysExactResultAndRejectsChangedCart(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 10, 20)
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := financedCorrectionUC(f)
	detail, err := uc.Detail(context.Background(), f.gymID, sale.SaleID)
	if err != nil {
		t.Fatal(err)
	}
	in := billingApp.CorrectSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
		ExpectedVersion: 0, Reason: "Se registró una pieza extra", MoneyResolution: "record_only",
		IdempotencyKey: "correction-exact-replay",
		Lines: []billingApp.CorrectSaleLineInput{{
			SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 1,
		}},
	}
	first, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay differs:\nfirst=%+v\nsecond=%+v", first, second)
	}
	var persisted struct {
		Fingerprint string `db:"idempotency_fingerprint"`
		Result      string `db:"idempotency_result"`
	}
	if err := f.db.Get(&persisted, `SELECT idempotency_fingerprint,idempotency_result FROM sale_corrections WHERE id=?`, first.CorrectionID.String()); err != nil {
		t.Fatal(err)
	}
	if persisted.Fingerprint == "" || !json.Valid([]byte(persisted.Result)) {
		t.Fatalf("persisted idempotency evidence = %+v", persisted)
	}

	changed := in
	changed.Lines = []billingApp.CorrectSaleLineInput{{
		SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 2,
	}}
	if _, err := uc.Execute(context.Background(), changed); !errors.Is(err, billingErrors.ErrIdempotencyKeyConflict) {
		t.Fatalf("changed cart with reused key error = %v, want idempotency conflict", err)
	}
	var corrections int
	_ = f.db.Get(&corrections, `SELECT COUNT(*) FROM sale_corrections WHERE idempotency_key=?`, in.IdempotencyKey)
	if corrections != 1 {
		t.Fatalf("corrections=%d, want 1", corrections)
	}
}

func TestCorrectSale_ConcurrentCommandsConvergeByKeyAndSaleVersion(t *testing.T) {
	run := func(t *testing.T, sameKey bool) {
		f := setupSales(t)
		productID := f.seedProduct(t, "Agua", 10, 20)
		sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
			Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
		})
		if err != nil {
			t.Fatal(err)
		}
		uc := financedCorrectionUC(f)
		detail, err := uc.Detail(context.Background(), f.gymID, sale.SaleID)
		if err != nil {
			t.Fatal(err)
		}
		base := billingApp.CorrectSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
			ExpectedVersion: 0, Reason: "Se registró una pieza extra", MoneyResolution: "record_only",
			IdempotencyKey: "concurrent-correction-a",
			Lines: []billingApp.CorrectSaleLineInput{{
				SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 1,
			}},
		}
		inputs := []billingApp.CorrectSaleInput{base, base}
		if !sameKey {
			inputs[1].IdempotencyKey = "concurrent-correction-b"
		}
		type result struct {
			out *billingApp.CorrectSaleOutput
			err error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for _, in := range inputs {
			in := in
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				out, err := uc.Execute(context.Background(), in)
				results <- result{out: out, err: err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		got := make([]result, 0, 2)
		for r := range results {
			got = append(got, r)
		}
		if sameKey {
			if got[0].err != nil || got[1].err != nil || !reflect.DeepEqual(got[0].out, got[1].out) {
				t.Fatalf("same-key results = %+v, want two exact successful replays", got)
			}
		} else {
			successes, conflicts := 0, 0
			for _, r := range got {
				if r.err == nil {
					successes++
				} else if errors.Is(r.err, billingErrors.ErrSaleVersionConflict) {
					conflicts++
				}
			}
			if successes != 1 || conflicts != 1 {
				t.Fatalf("different-key results = %+v, want one success and one version conflict", got)
			}
		}
		var corrections int
		_ = f.db.Get(&corrections, `SELECT COUNT(*) FROM sale_corrections WHERE sale_id=?`, sale.SaleID.String())
		if corrections != 1 {
			t.Fatalf("persisted corrections=%d, want 1", corrections)
		}
	}
	t.Run("same key replays", func(t *testing.T) { run(t, true) })
	t.Run("different keys conflict by version", func(t *testing.T) { run(t, false) })
}

func TestCorrectSale_IncreaseRequiresExplicitResolutionAndTracksWhereMoneyWent(t *testing.T) {
	tests := []struct {
		name                    string
		resolution              string
		role                    string
		method                  string
		wantAmount, wantBalance int64
		wantNow, wantPending    float64
		wantHistorical          float64
		wantSettlements         int
	}{
		{name: "pending", resolution: "pending", role: "operator", wantAmount: 1000, wantBalance: 1000, wantPending: 10},
		{name: "already collected", resolution: "already_collected", role: "owner", wantAmount: 2000, wantHistorical: 10},
		{name: "collect now", resolution: "collect_now", role: "owner", method: "transfer", wantAmount: 1000, wantNow: 10, wantSettlements: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := setupSales(t)
			productID := f.seedProduct(t, "Agua", 10, 20)
			sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
				GymID: f.gymID, ActorUserID: f.ownerID, MemberID: &f.memberID, Method: "cash",
				Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			uc := financedCorrectionUC(f)
			detail, _ := uc.Detail(context.Background(), f.gymID, sale.SaleID)
			in := billingApp.CorrectSaleInput{
				GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: tc.role, SaleID: sale.SaleID,
				ExpectedVersion: 0, Reason: "En realidad eran dos piezas", MoneyResolution: "record_only",
				IncreaseResolution: tc.resolution, CollectionMethod: tc.method,
				IdempotencyKey: "increase-" + tc.resolution,
				Lines: []billingApp.CorrectSaleLineInput{{
					SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 2,
				}},
			}
			out, err := uc.Execute(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if out.MoneyEffect.AdditionalCollectedNow != tc.wantNow || out.MoneyEffect.AdditionalPending != tc.wantPending ||
				out.MoneyEffect.AdditionalHistoricalCollected != tc.wantHistorical {
				t.Fatalf("additional effect = %+v", out.MoneyEffect)
			}
			var root struct{ Amount, Recognized, Balance int64 }
			if err := f.db.Get(&root, `SELECT amount,recognized_amount AS recognized,balance_pending AS balance FROM payments WHERE id=?`, sale.PaymentID.String()); err != nil {
				t.Fatal(err)
			}
			if root.Amount != tc.wantAmount || root.Balance != tc.wantBalance {
				t.Fatalf("root cents = %+v, want amount=%d balance=%d", root, tc.wantAmount, tc.wantBalance)
			}
			var settlements int
			_ = f.db.Get(&settlements, `SELECT COUNT(*) FROM payments WHERE parent_payment_id=? AND concept='balance_settlement'`, sale.PaymentID.String())
			if settlements != tc.wantSettlements {
				t.Fatalf("settlements=%d, want %d", settlements, tc.wantSettlements)
			}
		})
	}
}

func TestCorrectSale_IncreasePendingRejectsWalkInAndNonOwnerPhysicalEffects(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 10, 20)
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := financedCorrectionUC(f)
	detail, _ := uc.Detail(context.Background(), f.gymID, sale.SaleID)
	base := billingApp.CorrectSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "operator", SaleID: sale.SaleID,
		ExpectedVersion: 0, Reason: "En realidad eran dos piezas", MoneyResolution: "record_only",
		IncreaseResolution: "pending", IdempotencyKey: "walkin-pending-increase",
		Lines: []billingApp.CorrectSaleLineInput{{SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 2}},
	}
	if _, err := uc.Execute(context.Background(), base); !errors.Is(err, billingErrors.ErrCreditRequiresMember) {
		t.Fatalf("walk-in pending error=%v, want member requirement", err)
	}
	physical := base
	physical.IncreaseResolution = "already_collected"
	physical.IdempotencyKey = "operator-historical-increase"
	if _, err := uc.Execute(context.Background(), physical); !errors.Is(err, billingErrors.ErrSaleCorrectionForbidden) {
		t.Fatalf("operator physical correction error=%v, want forbidden", err)
	}
}

func TestCorrectSale_AnnulDistinguishesCaptureErrorFromRealSaleRefund(t *testing.T) {
	t.Run("capture never existed tombstones economic payment without refund", func(t *testing.T) {
		f := setupSales(t)
		productID := f.seedProduct(t, "Agua", 10, 20)
		sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
			Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := financedCorrectionUC(f).Execute(context.Background(), billingApp.CorrectSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
			ExpectedVersion: 0, Annul: true, Reason: "La venta nunca ocurrió", MoneyResolution: "record_only",
			IdempotencyKey: "annul-capture-error",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Annulled || out.CorrectionType != "annul" || out.MoneyEffect.RecognizedIncome != 0 || out.MoneyEffect.RefundedNow != 0 {
			t.Fatalf("annul output = %+v", out)
		}
		var saleDeleted, paymentDeleted *int64
		_ = f.db.Get(&saleDeleted, `SELECT deleted_at FROM sales WHERE id=?`, sale.SaleID.String())
		_ = f.db.Get(&paymentDeleted, `SELECT deleted_at FROM payments WHERE id=?`, sale.PaymentID.String())
		if saleDeleted == nil || paymentDeleted == nil {
			t.Fatalf("sale/payment tombstones = %v/%v", saleDeleted, paymentDeleted)
		}
		var stock int
		_ = f.db.Get(&stock, `SELECT stock FROM products WHERE id=?`, productID.String())
		if stock != 20 {
			t.Fatalf("restored stock=%d, want 20", stock)
		}
		var refunds int
		_ = f.db.Get(&refunds, `SELECT COUNT(*) FROM refunds WHERE correction_id=?`, out.CorrectionID.String())
		if refunds != 0 {
			t.Fatalf("capture error created %d refunds, want none", refunds)
		}
	})

	t.Run("real collection removes income and records non-economic settlement", func(t *testing.T) {
		f := setupSales(t)
		productID := f.seedProduct(t, "Agua", 10, 20)
		sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
			Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := financedCorrectionUC(f).Execute(context.Background(), billingApp.CorrectSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
			ExpectedVersion: 0, Annul: true, Reason: "Captura duplicada con dinero real", MoneyResolution: "refund_excess",
			RefundMethod: "cash", IdempotencyKey: "annul-real-collection",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Annulled || out.MoneyEffect.RefundedNow != 20 || out.MoneyEffect.RecognizedIncome != 0 {
			t.Fatalf("annul/refund output = %+v", out)
		}
		var row struct {
			Kind       string
			Amount     int64
			Recognized int64
			DeletedAt  *int64 `db:"deleted_at"`
		}
		if err := f.db.Get(&row, `SELECT r.kind,r.amount,p.recognized_amount AS recognized,p.deleted_at
			FROM refunds r JOIN payments p ON p.id=r.root_payment_id WHERE r.correction_id=?`, out.CorrectionID.String()); err != nil {
			t.Fatal(err)
		}
		if row.Kind != "overcollection_settlement" || row.Amount != 2000 || row.Recognized != 0 || row.DeletedAt != nil {
			t.Fatalf("annul settlement evidence = %+v", row)
		}
	})

	t.Run("pending return remains settleable after the sale tombstone", func(t *testing.T) {
		f := setupSales(t)
		productID := f.seedProduct(t, "Agua", 10, 20)
		sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
			Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
		})
		if err != nil {
			t.Fatal(err)
		}
		uc := financedCorrectionUC(f)
		annulled, err := uc.Execute(context.Background(), billingApp.CorrectSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
			ExpectedVersion: 0, Annul: true, Reason: "Captura duplicada; devolver después", MoneyResolution: "refund_pending",
			IdempotencyKey: "annul-pending-return",
		})
		if err != nil {
			t.Fatal(err)
		}
		if annulled.MoneyEffect.PendingRefundDue != 20 {
			t.Fatalf("pending refund=%v, want 20", annulled.MoneyEffect.PendingRefundDue)
		}
		settled, err := uc.SettlePending(context.Background(), billingApp.SettlePendingRefundInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", CorrectionID: annulled.CorrectionID,
			Method: "transfer", IdempotencyKey: "settle-annul-pending-return",
		})
		if err != nil {
			t.Fatalf("settle pending annul: %v", err)
		}
		if settled.Amount != 20 || settled.PendingAmount != 0 {
			t.Fatalf("settled pending annul = %+v", settled)
		}
	})
}

func TestCorrectSale_ReplacingProductReportsBothInventoryEffects(t *testing.T) {
	f := setupSales(t)
	oldProduct := f.seedProduct(t, "Agua", 10, 20)
	newProduct := f.seedProduct(t, "Suero", 15, 20)
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: oldProduct, Quantity: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := financedCorrectionUC(f)
	detail, _ := uc.Detail(context.Background(), f.gymID, sale.SaleID)
	out, err := uc.Execute(context.Background(), billingApp.CorrectSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
		ExpectedVersion: 0, Reason: "Era suero, no agua", MoneyResolution: "record_only",
		IncreaseResolution: "already_collected", IdempotencyKey: "replace-product",
		Lines: []billingApp.CorrectSaleLineInput{{SaleItemID: &detail.Lines[0].SaleItemID, ProductID: newProduct, Quantity: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.InventoryEffects) != 2 {
		t.Fatalf("inventory effects = %+v, want restore old and deduct new", out.InventoryEffects)
	}
	deltas := map[string]int{}
	for _, effect := range out.InventoryEffects {
		deltas[effect.ProductID.String()] = effect.StockDelta
	}
	if deltas[oldProduct.String()] != 1 || deltas[newProduct.String()] != -1 {
		t.Fatalf("replacement deltas = %+v", deltas)
	}
}

func TestCorrectSale_ImmediateCashRefundUsesCorrectionDateAndCurrentDrawer(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 10, 10)
	originalDrawer, err := cashcloseDomain.NewDrawer(f.gymID, "Recepción 2", "test-original-drawer", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := billingRepoLite.NewCashDrawerSQLiteRepository().Create(tx, originalDrawer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	originalDay := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", CashDrawerID: &originalDrawer.ID,
		PaymentDate: originalDay,
		Items:       []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := financedCorrectionUC(f).WithGyms(f.gymRepo)
	detail, _ := uc.Detail(context.Background(), f.gymID, sale.SaleID)
	out, err := uc.Execute(context.Background(), billingApp.CorrectSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
		ExpectedVersion: 0, Reason: "Se cobró una pieza de más", MoneyResolution: "refund_excess",
		RefundMethod: "cash", RefundCashDrawerID: &f.gymID, IdempotencyKey: "corr-refund-current-drawer",
		Lines: []billingApp.CorrectSaleLineInput{{
			SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 1,
		}},
	})
	if err != nil {
		t.Fatalf("correction refund: %v", err)
	}
	if out.MoneyEffect.RefundID == nil {
		t.Fatal("missing refund id")
	}
	var persisted struct {
		RefundedOn string `db:"refunded_on"`
		DrawerID   string `db:"cash_drawer_id"`
	}
	if err := f.db.Get(&persisted, `
		SELECT r.refunded_on,p.cash_drawer_id
		FROM refunds r JOIN payments p ON p.id=r.refund_payment_id
		WHERE p.id=?`, out.MoneyEffect.RefundID.String()); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/Mexico_City")
	wantDay := time.Now().In(loc).Format("2006-01-02")
	if persisted.RefundedOn != wantDay || persisted.RefundedOn == originalDay.Format("2006-01-02") {
		t.Fatalf("refund day=%s, want correction local day=%s (not original %s)", persisted.RefundedOn, wantDay, originalDay.Format("2006-01-02"))
	}
	if persisted.DrawerID != f.gymID.String() || persisted.DrawerID == originalDrawer.ID.String() {
		t.Fatalf("refund drawer=%s, want current/main=%s", persisted.DrawerID, f.gymID)
	}
}

func TestCorrectSale_PreservesEffectiveDiscountRateDownToSmallCart(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Dulce", 1, 50)
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", Discount: 4,
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 40}},
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := financedCorrectionUC(f)
	detail, err := uc.Detail(context.Background(), f.gymID, sale.SaleID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := uc.Execute(context.Background(), billingApp.CorrectSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
		ExpectedVersion: 0, Reason: "Sólo eran cuatro piezas", MoneyResolution: "record_only",
		IdempotencyKey: "corr-discount-40-to-4",
		Lines: []billingApp.CorrectSaleLineInput{{
			SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 4,
		}},
	})
	if err != nil {
		t.Fatalf("correct discounted sale: %v", err)
	}
	if out.MoneyEffect.CorrectedTotal != 3.60 {
		t.Fatalf("corrected total=%v, want 3.60", out.MoneyEffect.CorrectedTotal)
	}
	var persisted struct{ Subtotal, Discount, Total int64 }
	if err := f.db.Get(&persisted, `SELECT subtotal,discount,total FROM sales WHERE id=?`, sale.SaleID.String()); err != nil {
		t.Fatal(err)
	}
	if persisted.Subtotal != 400 || persisted.Discount != 40 || persisted.Total != 360 {
		t.Fatalf("sale cents=%+v, want 400/40/360", persisted)
	}
	var payment struct {
		Amount, Recognized, Discount int64
		Breakdown                    string
	}
	if err := f.db.Get(&payment, `SELECT amount,recognized_amount AS recognized,discount_amount AS discount,breakdown FROM payments WHERE id=?`, sale.PaymentID.String()); err != nil {
		t.Fatal(err)
	}
	if payment.Amount != 360 || payment.Recognized != 360 || payment.Discount != 40 {
		t.Fatalf("payment cents=%+v, want 360/360/40", payment)
	}
	var lines []map[string]any
	if err := json.Unmarshal([]byte(payment.Breakdown), &lines); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("breakdown should contain product subtotal only, got %s", payment.Breakdown)
	}
	var snapshots struct{ Before, After string }
	if err := f.db.Get(&snapshots, `SELECT before_snapshot AS before,after_snapshot AS after FROM sale_corrections WHERE id=?`, out.CorrectionID.String()); err != nil {
		t.Fatal(err)
	}
	if !containsJSONCents(snapshots.Before, "discount_cents", 400) || !containsJSONCents(snapshots.After, "discount_cents", 40) {
		t.Fatalf("discount evidence missing: before=%s after=%s", snapshots.Before, snapshots.After)
	}
}

func containsJSONCents(raw, key string, want float64) bool {
	var payload map[string]any
	return json.Unmarshal([]byte(raw), &payload) == nil && payload[key] == want
}

func financedCorrectionUC(f *salesFixture) *billingApp.CorrectSale {
	return billingApp.NewCorrectSale(f.saleRepo, f.saleItemRepo, f.paymentRepo,
		billingRepoLite.NewSaleCorrectionSQLiteRepository(), billingRepoLite.NewRefundSQLiteRepository(),
		f.productSvc, f.folios, nil, f.uow, f.recorder).WithGyms(f.gymRepo)
}

func financedRefundUC(f *salesFixture) *billingApp.RefundSale {
	refunds := billingRepoLite.NewRefundSQLiteRepository()
	refundPayment := billingApp.NewRefundPayment(f.paymentRepo, f.folios, f.memberSvc, f.uow, f.recorder).
		WithRefunds(refunds).
		WithSaleDetails(f.saleRepo, f.saleItemRepo, f.productSvc).
		WithGyms(f.gymRepo)
	return billingApp.NewRefundSale(f.saleRepo, refundPayment, f.uow)
}

func TestCorrectFinancedSale_CashBasisKeepsCollectedAndRecalculatesDebt(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Barra", 10, 30)
	paid := 40.0
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: &f.memberID,
		Method: "cash", Paid: &paid,
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := financedCorrectionUC(f)
	before, err := uc.Detail(context.Background(), f.gymID, sale.SaleID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := uc.Execute(context.Background(), billingApp.CorrectSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
		ExpectedVersion: 0, Reason: "Se capturaron dos piezas de más", MoneyResolution: "record_only",
		IdempotencyKey: "corr-financed-80",
		Lines:          []billingApp.CorrectSaleLineInput{{SaleItemID: &before.Lines[0].SaleItemID, ProductID: productID, Quantity: 8}},
	})
	if err != nil {
		t.Fatalf("correct financed sale: %v", err)
	}
	if out.MoneyEffect.CorrectedTotal != 80 || out.MoneyEffect.PhysicalCollected != 40 ||
		out.MoneyEffect.RecognizedIncome != 40 || out.MoneyEffect.PendingRefundDue != 0 {
		t.Fatalf("money effect = %+v", out.MoneyEffect)
	}
	var row struct {
		Amount, Recognized, Balance int64
	}
	if err := f.db.Get(&row, `SELECT amount,recognized_amount AS recognized,balance_pending AS balance FROM payments WHERE id=?`, sale.PaymentID.String()); err != nil {
		t.Fatal(err)
	}
	if row.Amount != 4000 || row.Recognized != 4000 || row.Balance != 4000 {
		t.Fatalf("payment cents = %+v, want 4000/4000/4000", row)
	}
}

func TestCorrectFinancedSale_RefundPendingOnlyTracksCollectedExcess(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Barra", 10, 30)
	paid := 40.0
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: &f.memberID,
		Method: "cash", Paid: &paid,
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := financedCorrectionUC(f)
	detail, _ := uc.Detail(context.Background(), f.gymID, sale.SaleID)
	out, err := uc.Execute(context.Background(), billingApp.CorrectSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
		ExpectedVersion: 0, Reason: "Sólo eran tres piezas", MoneyResolution: "refund_pending",
		IdempotencyKey: "corr-financed-30",
		Lines:          []billingApp.CorrectSaleLineInput{{SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 3}},
	})
	if err != nil {
		t.Fatalf("correct financed sale: %v", err)
	}
	if out.MoneyEffect.CorrectedTotal != 30 || out.MoneyEffect.PhysicalCollected != 40 ||
		out.MoneyEffect.RecognizedIncome != 30 || out.MoneyEffect.PendingRefundDue != 10 {
		t.Fatalf("money effect = %+v", out.MoneyEffect)
	}
	var payment struct{ Amount, Recognized, Balance int64 }
	_ = f.db.Get(&payment, `SELECT amount,recognized_amount AS recognized,balance_pending AS balance FROM payments WHERE id=?`, sale.PaymentID.String())
	if payment.Amount != 4000 || payment.Recognized != 3000 || payment.Balance != 0 {
		t.Fatalf("payment cents = %+v", payment)
	}
	var monetaryDelta int64
	_ = f.db.Get(&monetaryDelta, `SELECT monetary_delta FROM sale_corrections WHERE id=?`, out.CorrectionID.String())
	if monetaryDelta != 1000 {
		t.Fatalf("pending monetary delta=%d, want 1000", monetaryDelta)
	}
}

func TestRefundFinancedSale_CancelsDebtBeforeReturningMoney(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Barra", 10, 30)
	paid := 40.0
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: &f.memberID,
		Method: "cash", Paid: &paid,
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	corrections := financedCorrectionUC(f)
	detail, err := corrections.Detail(context.Background(), f.gymID, sale.SaleID)
	if err != nil {
		t.Fatal(err)
	}
	lineID := detail.Lines[0].SaleItemID
	uc := financedRefundUC(f)
	first, err := uc.Execute(context.Background(), billingApp.RefundSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, SaleID: sale.SaleID,
		Reason: "Devolvió tres piezas", IdempotencyKey: "return-debt-only",
		Items: []refundDomain.ItemInput{{SaleItemID: lineID, Quantity: 3, Disposition: refundDomain.Damaged}},
	})
	if err != nil {
		t.Fatalf("debt-only return: %v", err)
	}
	if first.Amount != 0 || first.BalanceCancelled != 30 {
		t.Fatalf("first refund = %+v, want cash=0 debt=30", first)
	}
	var firstRow struct {
		Amount, Cancelled int64
		PaymentID         *string `db:"refund_payment_id"`
		Method            *string `db:"method"`
	}
	if err := f.db.Get(&firstRow, `SELECT amount,balance_cancelled AS cancelled,refund_payment_id,method FROM refunds WHERE id=?`, first.RefundID.String()); err != nil {
		t.Fatal(err)
	}
	if firstRow.Amount != 0 || firstRow.Cancelled != 3000 || firstRow.PaymentID != nil || firstRow.Method != nil {
		t.Fatalf("debt-only persisted = %+v", firstRow)
	}

	second, err := uc.Execute(context.Background(), billingApp.RefundSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, SaleID: sale.SaleID,
		Reason: "Devolvió cinco piezas más", Method: "cash", IdempotencyKey: "return-debt-plus-cash",
		Items: []refundDomain.ItemInput{{SaleItemID: lineID, Quantity: 5, Disposition: refundDomain.Damaged}},
	})
	if err != nil {
		t.Fatalf("mixed return: %v", err)
	}
	if second.Amount != 20 || second.BalanceCancelled != 30 {
		t.Fatalf("second refund = %+v, want cash=20 debt=30", second)
	}
	var balance int64
	_ = f.db.Get(&balance, `SELECT balance_pending FROM payments WHERE id=?`, sale.PaymentID.String())
	if balance != 0 {
		t.Fatalf("remaining debt=%d, want 0", balance)
	}
	var refundPaymentAmount int64
	_ = f.db.Get(&refundPaymentAmount, `SELECT amount FROM payments WHERE concept='refund' AND parent_payment_id=?`, sale.PaymentID.String())
	if refundPaymentAmount != -2000 {
		t.Fatalf("physical refund cents=%d, want -2000", refundPaymentAmount)
	}
	// One coalesced root Payment participates in both refunds. Persisted graph
	// metadata must retain both ids so BatchSize can never split either command.
	var rootGraphIDs int
	if err := f.db.Get(&rootGraphIDs, `SELECT COUNT(*)
		FROM sync_queue q,json_each(q.payload,'$._sync_graph_ids') graph
		WHERE q.entity_type='payments' AND q.entity_id=? AND q.synced_at IS NULL
		  AND graph.value IN (?,?)`, sale.PaymentID.String(), first.RefundID.String(), second.RefundID.String()); err != nil {
		t.Fatal(err)
	}
	if rootGraphIDs != 2 {
		t.Fatalf("root payment carries %d refund graph ids, want 2", rootGraphIDs)
	}
	for _, refundID := range []string{first.RefundID.String(), second.RefundID.String()} {
		var metadata struct {
			Expected int `db:"expected"`
			Tagged   int `db:"tagged"`
		}
		if err := f.db.Get(&metadata, `SELECT
			CAST(json_extract(payload,'$._sync_graph_expected_items') AS INTEGER) AS expected,
			EXISTS(SELECT 1 FROM json_each(payload,'$._sync_graph_ids') WHERE value=?) AS tagged
			FROM sync_queue WHERE entity_type='refunds' AND entity_id=? AND synced_at IS NULL`,
			refundID, refundID); err != nil {
			t.Fatal(err)
		}
		if metadata.Expected != 1 || metadata.Tagged != 1 {
			t.Fatalf("refund %s graph metadata=%+v, want one tagged item", refundID, metadata)
		}
	}
}

func TestRefundDiscountedSale_SequentialPartialsTelescopeToExactTotal(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Muestra", 1, 3)
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", Discount: 1,
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lineID := sale.Items[0].SaleItemID
	uc := financedRefundUC(f)
	wants := []float64{0.67, 0.66, 0.67}
	var total float64
	for i, want := range wants {
		out, err := uc.Execute(context.Background(), billingApp.RefundSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, SaleID: sale.SaleID,
			Reason: "Devolución parcial", Method: "cash",
			IdempotencyKey: "discounted-partial-" + string(rune('1'+i)),
			Items:          []refundDomain.ItemInput{{SaleItemID: lineID, Quantity: 1, Disposition: refundDomain.Damaged}},
		})
		if err != nil {
			t.Fatalf("partial %d: %v", i+1, err)
		}
		if out.Amount != want {
			t.Fatalf("partial %d amount=%v, want %v", i+1, out.Amount, want)
		}
		total += out.Amount
	}
	if total != sale.Total {
		t.Fatalf("partial refunds total=%v, want discounted sale total=%v", total, sale.Total)
	}
	var cents int64
	if err := f.db.Get(&cents, `SELECT SUM(amount) FROM refunds WHERE sale_id=?`, sale.SaleID.String()); err != nil {
		t.Fatal(err)
	}
	if cents != 200 {
		t.Fatalf("persisted refund cents=%d, want 200", cents)
	}
}
