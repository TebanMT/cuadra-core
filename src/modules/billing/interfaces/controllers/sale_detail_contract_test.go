package controllers

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestSaleDetailContractExposesNullableMemberID(t *testing.T) {
	memberID := uuid.New()
	payload, err := json.Marshal(saleDetailResp{ID: uuid.New(), PaymentID: uuid.New(), MemberID: &memberID})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["member_id"] != memberID.String() {
		t.Fatalf("member_id=%v, want %s", wire["member_id"], memberID)
	}

	payload, err = json.Marshal(saleDetailResp{ID: uuid.New(), PaymentID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if value, exists := wire["member_id"]; !exists || value != nil {
		t.Fatalf("walk-in member_id=%v exists=%v, want explicit null", value, exists)
	}
}

func TestPaymentListContractExposesVersionAndLinkedSaleID(t *testing.T) {
	saleID := uuid.New()
	payload, err := json.Marshal(paymentResp{ID: uuid.New(), Version: 3, SaleID: &saleID})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["version"] != float64(3) || wire["sale_id"] != saleID.String() {
		t.Fatalf("payment wire=%v, want version=3 sale_id=%s", wire, saleID)
	}
}
