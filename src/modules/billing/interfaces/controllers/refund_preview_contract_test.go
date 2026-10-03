package controllers

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestRefundPreviewResponseContract(t *testing.T) {
	selected, root := uuid.New(), uuid.New()
	payload, err := json.Marshal(refundPreviewResp{
		SelectedPaymentID: selected, RootPaymentID: root,
		SelectedRefundable: 40, AggregateCollected: 140,
		AggregateRefunded: 0, AggregateRefundable: 140,
		BalancePending: 460, RevertMembershipTotal: 600,
		MembershipRevertAllowed: false,
		MembershipRevertReason:  "ajuste manual requerido",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"selected_payment_id", "root_payment_id", "selected_refundable",
		"aggregate_collected", "aggregate_refunded", "aggregate_refundable",
		"balance_pending", "revert_membership_total", "membership_revert_allowed",
		"membership_revert_block_reason",
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("missing JSON field %q in %s", key, payload)
		}
	}
}
