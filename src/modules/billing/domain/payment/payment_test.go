package payment

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ptr[T any](v T) *T { return &v }

func TestNewMembershipPayment_HappyPath(t *testing.T) {
	id, gym, op, mem := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	p, err := NewMembershipPayment(id, gym, op, mem, "MEM-000001",
		500, 0, 500, 0, MethodCash, now, now, nil, nil)
	if err != nil {
		t.Fatalf("ok payment: %v", err)
	}
	if p.Amount != 500 || p.BalancePending != 0 || p.Concept != ConceptMembership {
		t.Errorf("payment shape = %+v", p)
	}
	if p.MemberID == nil || *p.MemberID != mem {
		t.Errorf("member id mismatch")
	}
	if p.Folio != "MEM-000001" {
		t.Errorf("folio = %s", p.Folio)
	}
}

func TestNewMembershipPayment_PartialPaymentExtendsBalance(t *testing.T) {
	id, gym, op, mem := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	p, err := NewMembershipPayment(id, gym, op, mem, "MEM-000001",
		500, 0, 300, 200, MethodCash, now, now, nil, nil)
	if err != nil {
		t.Fatalf("partial: %v", err)
	}
	if p.Amount != 300 || p.BalancePending != 200 {
		t.Errorf("partial shape = amount=%v balance=%v", p.Amount, p.BalancePending)
	}
}

func TestNewMembershipPayment_DiscountRequiresReason(t *testing.T) {
	now := time.Now().UTC()
	_, err := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 100, 400, 0, MethodCash, now, now, nil, nil)
	if err == nil {
		t.Errorf("discount without reason should fail")
	}
	r := "promo amigos"
	p, err := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 100, 400, 0, MethodCash, now, now, nil, &r)
	if err != nil {
		t.Fatalf("with reason: %v", err)
	}
	if p.DiscountAmount != 100 || p.DiscountReason == nil || *p.DiscountReason != "promo amigos" {
		t.Errorf("discount missing on payment: %+v", p)
	}
}

func TestNewMembershipPayment_RejectsBadMethod(t *testing.T) {
	now := time.Now().UTC()
	_, err := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 0, 500, 0, "", now, now, nil, nil)
	if err == nil {
		t.Errorf("missing method should fail")
	}
	_, err = NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 0, 500, 0, "bitcoin", now, now, nil, nil)
	if err == nil {
		t.Errorf("invalid method should fail")
	}
}

func TestNewMembershipPayment_RejectsBadAmounts(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name                              string
		subtotal, discount, paid, balance float64
	}{
		{"subtotal zero", 0, 0, 0, 0},
		{"subtotal negative", -1, 0, 0, 0},
		{"discount equal to subtotal", 100, 100, 0, 0},
		{"paid greater than total", 100, 0, 200, 0},
		{"paid + balance != total", 100, 0, 60, 50},
		{"paid zero", 100, 0, 0, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := "x"
			rPtr := &r
			if tc.discount == 0 {
				rPtr = nil
			}
			_, err := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(),
				"MEM-1", tc.subtotal, tc.discount, tc.paid, tc.balance,
				MethodCash, now, now, nil, rPtr)
			if err == nil {
				t.Errorf("expected failure")
			}
		})
	}
}

func TestNewBalanceSettlement_DecrementsParent(t *testing.T) {
	now := time.Now().UTC()
	mem := uuid.New()
	parent, _ := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), mem, "MEM-1",
		500, 0, 300, 200, MethodCash, now, now, nil, nil)
	settlement, err := NewBalanceSettlementPayment(uuid.New(), parent.GymID, uuid.New(),
		parent, "BAL-1", 150, MethodTransfer, now, now, nil)
	if err != nil {
		t.Fatalf("settlement: %v", err)
	}
	if settlement.Concept != ConceptBalanceSettlement || settlement.ParentPaymentID == nil || *settlement.ParentPaymentID != parent.ID {
		t.Errorf("settlement shape: %+v", settlement)
	}
	rem, err := parent.DecrementBalance(150, now)
	if err != nil {
		t.Fatalf("decrement: %v", err)
	}
	if rem != 50 {
		t.Errorf("remaining = %v, want 50", rem)
	}
}

func TestNewBalanceSettlement_RejectsExceedingBalance(t *testing.T) {
	now := time.Now().UTC()
	parent, _ := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 0, 300, 200, MethodCash, now, now, nil, nil)
	if _, err := NewBalanceSettlementPayment(uuid.New(), parent.GymID, uuid.New(),
		parent, "BAL-1", 250, MethodCash, now, now, nil); err == nil {
		t.Errorf("settlement over balance should fail")
	}
}

func TestNewRefundPayment_NegativeAmountAndReason(t *testing.T) {
	now := time.Now().UTC()
	parent, _ := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 0, 500, 0, MethodCash, now, now, nil, nil)
	refund, err := NewRefundPayment(uuid.New(), parent.GymID, uuid.New(),
		parent, "RFD-1", 500, MethodCash, "Cliente cambió de opinión", now, now)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if refund.Amount >= 0 {
		t.Errorf("refund amount should be negative, got %v", refund.Amount)
	}
	if refund.Notes == nil || !strings.Contains(*refund.Notes, "Cliente") {
		t.Errorf("refund notes missing reason: %v", refund.Notes)
	}
}

func TestNewRefundPayment_RejectsNoReason(t *testing.T) {
	now := time.Now().UTC()
	parent, _ := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 0, 500, 0, MethodCash, now, now, nil, nil)
	if _, err := NewRefundPayment(uuid.New(), parent.GymID, uuid.New(),
		parent, "RFD-1", 500, MethodCash, "  ", now, now); err == nil {
		t.Errorf("blank reason must fail")
	}
}

func TestNewRefundPayment_CannotRefundARefund(t *testing.T) {
	now := time.Now().UTC()
	parent, _ := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 0, 500, 0, MethodCash, now, now, nil, nil)
	refund, _ := NewRefundPayment(uuid.New(), parent.GymID, uuid.New(),
		parent, "RFD-1", 500, MethodCash, "x", now, now)
	if _, err := NewRefundPayment(uuid.New(), refund.GymID, uuid.New(),
		refund, "RFD-2", 500, MethodCash, "y", now, now); err == nil {
		t.Errorf("refund of refund must fail")
	}
}

func TestPayment_RecordPartialPayment(t *testing.T) {
	now := time.Now().UTC()
	p, _ := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 0, 500, 0, MethodCash, now, now, nil, nil)
	prevVer := p.Version
	if err := p.RecordPartialPayment(300, 200, now.Add(time.Second)); err != nil {
		t.Fatalf("record partial: %v", err)
	}
	if p.Amount != 300 || p.BalancePending != 200 {
		t.Errorf("partial state: %+v", p)
	}
	if p.Version != prevVer+1 {
		t.Errorf("version not bumped")
	}
}

func TestPayment_ApplyDiscount(t *testing.T) {
	now := time.Now().UTC()
	p, _ := NewMembershipPayment(uuid.New(), uuid.New(), uuid.New(), uuid.New(), "MEM-1",
		500, 0, 500, 0, MethodCash, now, now, nil, nil)
	if err := p.ApplyDiscount(100, "Promo"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if p.DiscountAmount != 100 {
		t.Errorf("discount = %v", p.DiscountAmount)
	}
	if err := p.ApplyDiscount(0, "x"); err == nil {
		t.Errorf("zero amount must fail")
	}
	if err := p.ApplyDiscount(50, "  "); err == nil {
		t.Errorf("blank reason must fail")
	}
}
