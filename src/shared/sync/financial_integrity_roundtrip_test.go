//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// Pins the cloud-wire -> SQLite half of the offline round-trip for every new
// ADR-011 aggregate. The wire is pesos; each SQLite assertion is cents.
func TestFinancialIntegrityPull_RoundTripsMoneyAndForeignKeys(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	now := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	nowMS := now.UnixMilli()
	userID, productID := uuid.New(), uuid.New()
	memberID, membershipTypeID, membershipID := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.Exec(`INSERT INTO users(id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role,active)
		VALUES(?,?,1,?,?,'owner@roundtrip.test','x','Owner','owner',1)`, userID, gymID, nowMS, nowMS); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO products(id,gym_id,version,created_at,updated_at,name,price,stock,stock_minimum,active)
		VALUES(?,?,1,?,?,'Agua',1500,20,2,1)`, productID, gymID, nowMS, nowMS); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO membership_types(id,gym_id,version,created_at,updated_at,name,price,duration_days,active)
		VALUES(?,?,1,?,?,'Mensual',40000,30,1)`, membershipTypeID, gymID, nowMS, nowMS); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO members(id,gym_id,version,created_at,updated_at,folio,full_name,phone,status,created_by)
		VALUES(?,?,1,?,?,'M-1','Socio','5555555555','active',?)`, memberID, gymID, nowMS, nowMS, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memberships(id,gym_id,version,created_at,updated_at,member_id,membership_type_id,type_name_snapshot,price_snapshot,duration_days_snapshot,start_date,expiry_date,status)
		VALUES(?,?,1,?,?,?,?,'Mensual',40000,30,'2026-08-01','2026-08-30','active')`, membershipID, gymID, nowMS, nowMS, memberID, membershipTypeID); err != nil {
		t.Fatal(err)
	}

	rootPayment, overPayment, revenueRefundPayment, otherPayment := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	saleID, saleItemID := uuid.New(), uuid.New()
	correctionID, paymentCorrectionID := uuid.New(), uuid.New()
	overRefundID, revenueRefundID, refundItemID := uuid.New(), uuid.New(), uuid.New()
	stockMovementID, cashMovementID, purchaseID := uuid.New(), uuid.New(), uuid.New()
	base := func(id uuid.UUID) map[string]any {
		return map[string]any{
			"id": id.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": nowMS, "updated_at": nowMS, "deleted_at": nil,
		}
	}
	payment := func(id uuid.UUID, folio string, amount, recognized float64, parent any) map[string]any {
		p := base(id)
		p["folio"], p["amount"], p["recognized_amount"] = folio, amount, recognized
		p["payment_method"], p["concept"], p["parent_payment_id"] = "cash", "refund", parent
		if parent == nil {
			p["concept"] = "product"
		}
		p["discount_amount"], p["balance_pending"] = 0, 0
		p["payment_date"], p["operator_id"], p["cash_drawer_id"] = "2026-08-24", userID.String(), gymID.String()
		return p
	}
	rootPayload := payment(rootPayment, "PRD-1", 40, 4, nil)
	rootPayload["membership_id"] = membershipID.String()

	changes := []syncpkg.PullChange{
		change(t, "payments", rootPayment, rootPayload, now),
		change(t, "payments", overPayment, payment(overPayment, "RFD-1", -36, 0, rootPayment.String()), now.Add(time.Millisecond)),
		change(t, "payments", revenueRefundPayment, payment(revenueRefundPayment, "RFD-2", -2, 0, rootPayment.String()), now.Add(2*time.Millisecond)),
	}
	otherPayload := payment(otherPayment, "OTH-1", 12, 12, nil)
	otherPayload["concept"] = "other"
	changes = append(changes, change(t, "payments", otherPayment, otherPayload, now.Add(3*time.Millisecond)))
	paymentCorrection := base(paymentCorrectionID)
	paymentCorrection["payment_id"], paymentCorrection["expected_payment_version"] = otherPayment.String(), 1
	paymentCorrection["reason"] = "Método de pago capturado incorrectamente"
	paymentCorrection["before_snapshot"] = map[string]any{
		"version": 1, "amount": 12.0, "recognized_amount": 12.0, "balance_pending": 0,
		"payment_method": "cash", "cash_drawer_id": gymID.String(), "payment_date": "2026-08-24", "annulled": false,
	}
	paymentCorrection["after_snapshot"] = map[string]any{
		"version": 2, "amount": 12.0, "recognized_amount": 12.0, "balance_pending": 0,
		"payment_method": "transfer", "cash_drawer_id": nil, "payment_date": "2026-08-24", "annulled": false,
	}
	paymentCorrection["idempotency_key"] = "payment-correction-1"
	paymentCorrection["idempotency_fingerprint"] = "payment-correction-fingerprint-1"
	paymentCorrection["idempotency_result"] = map[string]any{"correction_id": paymentCorrectionID.String()}
	paymentCorrection["created_by"] = userID.String()
	changes = append(changes, change(t, "payment_corrections", paymentCorrectionID, paymentCorrection, now.Add(4*time.Millisecond)))
	sale := base(saleID)
	sale["payment_id"], sale["subtotal"], sale["discount"], sale["total"], sale["correction_version"] = rootPayment.String(), 4.0, 0, 4.0, 1
	changes = append(changes, change(t, "sales", saleID, sale, now.Add(5*time.Millisecond)))
	item := base(saleItemID)
	item["sale_id"], item["product_id"], item["product_name_snapshot"] = saleID.String(), productID.String(), "Agua"
	item["unit_price_snapshot"], item["unit_cost_snapshot"], item["quantity"], item["line_total"] = 4.0, 2.0, 1, 4.0
	changes = append(changes, change(t, "sale_items", saleItemID, item, now.Add(6*time.Millisecond)))
	corr := base(correctionID)
	corr["sale_id"], corr["expected_sale_version"], corr["reason"] = saleID.String(), 0, "Cantidad capturada mal"
	corr["correction_type"], corr["money_resolution"], corr["increase_resolution"], corr["monetary_delta"] = "edit", "refund_pending", nil, 36.0
	corr["before_snapshot"], corr["after_snapshot"] = map[string]any{"total_cents": 4000}, map[string]any{"total_cents": 400}
	corr["idempotency_key"], corr["idempotency_fingerprint"] = "corr-1", "corr-fingerprint-1"
	corr["idempotency_result"], corr["created_by"] = map[string]any{"correction_id": correctionID.String()}, userID.String()
	changes = append(changes, change(t, "sale_corrections", correctionID, corr, now.Add(7*time.Millisecond)))
	refund := func(id, paymentID uuid.UUID, kind string, correction any, amount float64) map[string]any {
		r := base(id)
		r["root_payment_id"], r["refund_payment_id"], r["sale_id"] = rootPayment.String(), paymentID.String(), saleID.String()
		r["amount"], r["method"], r["refunded_on"], r["reason"] = amount, "cash", "2026-08-24", "Devolución"
		r["kind"], r["balance_cancelled"], r["legacy_incomplete"], r["correction_id"] = kind, 0, false, correction
		r["idempotency_key"], r["idempotency_fingerprint"] = "refund-"+id.String(), "fingerprint-"+id.String()
		r["idempotency_result"], r["created_by"] = map[string]any{"refund_id": id.String()}, userID.String()
		return r
	}
	changes = append(changes,
		change(t, "refunds", overRefundID, refund(overRefundID, overPayment, "overcollection_settlement", correctionID.String(), 36), now.Add(8*time.Millisecond)),
		change(t, "refunds", revenueRefundID, refund(revenueRefundID, revenueRefundPayment, "revenue_refund", nil, 2), now.Add(9*time.Millisecond)))
	ri := base(refundItemID)
	ri["refund_id"], ri["sale_item_id"], ri["quantity"], ri["amount"], ri["disposition"] = revenueRefundID.String(), saleItemID.String(), 1, 2.0, "damaged"
	changes = append(changes, change(t, "refund_items", refundItemID, ri, now.Add(10*time.Millisecond)))
	sm := base(stockMovementID)
	sm["product_id"], sm["movement_type"], sm["delta"], sm["cost"] = productID.String(), "restock", 10, 3.0
	sm["is_purchase"], sm["operator_id"] = true, userID.String()
	sm["idempotency_key"], sm["idempotency_fingerprint"] = "adjust-1", "adjust-fingerprint-1"
	sm["idempotency_result"] = map[string]any{"new_stock": 30, "delta": 10, "movement_id": stockMovementID.String()}
	changes = append(changes, change(t, "stock_movements", stockMovementID, sm, now.Add(11*time.Millisecond)))
	cm := base(cashMovementID)
	cm["movement_on"], cm["amount"], cm["movement_type"], cm["reason"] = "2026-08-24", 30.0, "cash_out", "Compra inventario"
	cm["operator_id"], cm["classification_status"], cm["cash_drawer_id"] = userID.String(), "inventory_purchase", gymID.String()
	changes = append(changes, change(t, "cash_movements", cashMovementID, cm, now.Add(12*time.Millisecond)))
	ip := base(purchaseID)
	ip["stock_movement_id"], ip["product_id"], ip["quantity"] = stockMovementID.String(), productID.String(), 10
	ip["unit_cost"], ip["total_amount"], ip["status"], ip["paid_on"] = 3.0, 30.0, "paid", "2026-08-24"
	ip["payment_method"], ip["paid_from"], ip["cash_movement_id"] = "cash", "cash_drawer", cashMovementID.String()
	ip["idempotency_key"], ip["created_by"] = "purchase-1", userID.String()
	changes = append(changes, change(t, "inventory_purchases", purchaseID, ip, now.Add(13*time.Millisecond)))

	if err := syncpkg.ApplyPullPage(context.Background(), uow, changes, nil); err != nil {
		t.Fatalf("apply financial page: %v", err)
	}
	assertCents := func(query string, want int64, id uuid.UUID) {
		t.Helper()
		var got int64
		if err := db.Get(&got, query, id.String()); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", id, got, want)
		}
	}
	assertCents(`SELECT recognized_amount FROM payments WHERE id=?`, 400, rootPayment)
	var gotMembershipID string
	if err := db.Get(&gotMembershipID, `SELECT membership_id FROM payments WHERE id=?`, rootPayment.String()); err != nil || gotMembershipID != membershipID.String() {
		t.Fatalf("payment membership_id=%q, want %s (err=%v)", gotMembershipID, membershipID, err)
	}
	assertCents(`SELECT unit_cost_snapshot FROM sale_items WHERE id=?`, 200, saleItemID)
	assertCents(`SELECT monetary_delta FROM sale_corrections WHERE id=?`, 3600, correctionID)
	var correctionType string
	if err := db.Get(&correctionType, `SELECT correction_type FROM sale_corrections WHERE id=?`, correctionID.String()); err != nil || correctionType != "edit" {
		t.Fatalf("correction_type=%q err=%v", correctionType, err)
	}
	var correctionReplay string
	if err := db.Get(&correctionReplay, `SELECT idempotency_result FROM sale_corrections WHERE id=?`, correctionID.String()); err != nil || !json.Valid([]byte(correctionReplay)) {
		t.Fatalf("correction idempotency_result=%q err=%v", correctionReplay, err)
	}
	var paymentCorrectionReplay string
	if err := db.Get(&paymentCorrectionReplay, `SELECT idempotency_result FROM payment_corrections WHERE id=?`, paymentCorrectionID.String()); err != nil || !json.Valid([]byte(paymentCorrectionReplay)) {
		t.Fatalf("payment correction idempotency_result=%q err=%v", paymentCorrectionReplay, err)
	}
	assertCents(`SELECT amount FROM refunds WHERE id=?`, 3600, overRefundID)
	assertCents(`SELECT amount FROM refund_items WHERE id=?`, 200, refundItemID)
	assertCents(`SELECT total_amount FROM inventory_purchases WHERE id=?`, 3000, purchaseID)
	var stockReplay string
	if err := db.Get(&stockReplay, `SELECT idempotency_result FROM stock_movements WHERE id=?`, stockMovementID.String()); err != nil || !json.Valid([]byte(stockReplay)) {
		t.Fatalf("stock idempotency_result=%q err=%v", stockReplay, err)
	}
	var kind string
	if err := db.Get(&kind, `SELECT kind FROM refunds WHERE id=?`, overRefundID.String()); err != nil || kind != "overcollection_settlement" {
		t.Fatalf("kind=%q err=%v", kind, err)
	}
}

func TestFinancialIntegrityRegistry_IsTopologicalAndComplete(t *testing.T) {
	index := map[string]int{}
	for i, table := range syncpkg.SyncedTables {
		index[table.Type] = i
	}
	for _, typ := range []string{"inventory_purchases", "refunds", "refund_items", "sale_corrections", "payment_corrections"} {
		if syncpkg.FindTable(typ) == nil {
			t.Fatalf("missing %s", typ)
		}
	}
	if !(index["sales"] < index["sale_corrections"] && index["sale_corrections"] < index["refunds"] && index["refunds"] < index["refund_items"]) {
		t.Fatalf("correction/refund registry is not topological: %+v", index)
	}
	if !(index["payments"] < index["payment_corrections"]) {
		t.Fatalf("payment correction registry is not topological: %+v", index)
	}
	if !(index["stock_movements"] < index["inventory_purchases"] && index["cash_movements"] < index["inventory_purchases"]) {
		t.Fatalf("purchase registry is not topological: %+v", index)
	}
}

func change(t *testing.T, typ string, id uuid.UUID, payload map[string]any, at time.Time) syncpkg.PullChange {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return syncpkg.PullChange{EntityType: typ, EntityID: id.String(), Version: 1, Payload: raw, ServerUpdatedAt: at}
}
