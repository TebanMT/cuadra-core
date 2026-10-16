package sync

import (
	"encoding/json"
	"sort"
)

const financialMembersKey = "_sync_financial_members"

// Only command-owned financial rows participate. Product snapshots are excluded:
// independent sales share a product, while their immutable stock events commute.
func financialSyncType(kind string) bool {
	switch kind {
	case "members", "memberships", "membership_adjustments", "payments", "sales", "sale_items", "stock_movements", "applied_promotions", "payment_corrections", "sale_corrections", "refunds", "refund_items":
		return true
	}
	return false
}
func financialGraphItem(item PushItem) bool {
	return financialSyncType(item.EntityType) || len(financialMembers(item.Payload)) > 0 || len(refundGraphIDs(item.Payload)) > 0
}

func financialMembers(payload json.RawMessage) []string {
	raw := decodePushPayload(payload)
	out := []string{}
	if values, ok := raw[financialMembersKey].([]any); ok {
		for _, v := range values {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
func mergeFinancialMetadata(existing, incoming json.RawMessage) json.RawMessage {
	members := uniqueExpenseKeys(append(financialMembers(existing), financialMembers(incoming)...))
	if len(members) == 0 {
		return incoming
	}
	raw := decodePushPayload(incoming)
	if raw == nil {
		return incoming
	}
	raw[financialMembersKey] = members
	encoded, err := json.Marshal(raw)
	if err != nil {
		return incoming
	}
	return encoded
}

// Links also recover pending queues written by earlier desktops. A member is
// linked only through a command manifest; merely sharing a customer must not
// merge all of their unrelated transactions.
func financialKeys(item PushItem) []string {
	if !financialGraphItem(item) {
		return nil
	}
	keys := append([]string{expenseMemberKey(item.EntityType, item.EntityID)}, financialMembers(item.Payload)...)
	for _, id := range refundGraphIDs(item.Payload) {
		keys = append(keys, expenseMemberKey("refunds", id))
	}
	raw := decodePushPayload(item.Payload)
	link := func(kind, field string) {
		if id, ok := raw[field].(string); ok && id != "" {
			keys = append(keys, expenseMemberKey(kind, id))
		}
	}
	switch item.EntityType {
	case "payments":
		link("payments", "parent_payment_id")
		link("memberships", "membership_id")
	case "sales":
		link("payments", "payment_id")
	case "sale_items":
		link("sales", "sale_id")
	case "stock_movements":
		link("sale_items", "sale_item_id")
	case "membership_adjustments":
		link("memberships", "membership_id")
	case "applied_promotions", "payment_corrections":
		link("payments", "payment_id")
	case "sale_corrections":
		link("sales", "sale_id")
	case "refunds":
		link("payments", "root_payment_id")
		link("payments", "refund_payment_id")
		link("sales", "sale_id")
		link("sale_corrections", "correction_id")
	case "refund_items":
		link("refunds", "refund_id")
		link("sale_items", "sale_item_id")
	}
	// Replay responses contain the exact immutable IDs expected from a sale or
	// membership charge. json.Marshal on application outputs uses Go field names.
	if item.EntityType == "payments" {
		result, _ := raw["idempotency_result"].(map[string]any)
		if encoded, ok := raw["idempotency_result"].(string); ok {
			_ = json.Unmarshal([]byte(encoded), &result)
		}
		for field, kind := range map[string]string{"SaleID": "sales", "NewMembershipID": "memberships", "PromotionAppliedID": "applied_promotions"} {
			if id, ok := result[field].(string); ok && id != "" && id != "00000000-0000-0000-0000-000000000000" {
				keys = append(keys, expenseMemberKey(kind, id))
			}
		}
		if items, ok := result["Items"].([]any); ok {
			for _, v := range items {
				if row, ok := v.(map[string]any); ok {
					if id, ok := row["SaleItemID"].(string); ok {
						keys = append(keys, expenseMemberKey("sale_items", id))
					}
				}
			}
		}
	}
	return uniqueExpenseKeys(keys)
}
func groupFinancialItems(batch []PushItem) [][]int {
	parent := make([]int, len(batch))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	owners := map[string]int{}
	for i, item := range batch {
		for _, key := range financialKeys(item) {
			if owner, ok := owners[key]; ok {
				parent[find(i)] = find(owner)
			} else {
				owners[key] = i
			}
		}
	}
	groups := map[int][]int{}
	for i, item := range batch {
		if financialGraphItem(item) {
			groups[find(i)] = append(groups[find(i)], i)
		}
	}
	out := [][]int{}
	for _, indexes := range groups {
		out = append(out, indexes)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
