package sync

import (
	"encoding/json"
	"sort"
)

// Required members survive queue coalescing, including corrections which
// remove a link from the domain snapshot. They are transport metadata only.
const expenseGraphMembersKey = "_sync_expense_members"

func expenseSyncType(kind string) bool {
	return kind == "expenses" || kind == "cash_movements" || kind == "expense_occurrences"
}
func expenseMemberKey(kind, id string) string { return kind + "/" + id }
func expenseGraphMembers(payload json.RawMessage) []string {
	raw := decodePushPayload(payload)
	var out []string
	if members, ok := raw[expenseGraphMembersKey].([]any); ok {
		for _, member := range members {
			if key, ok := member.(string); ok {
				out = append(out, key)
			}
		}
	}
	return out
}
func expenseGraphKeys(item PushItem) []string {
	if !expenseSyncType(item.EntityType) {
		return nil
	}
	keys := append([]string{expenseMemberKey(item.EntityType, item.EntityID)}, expenseGraphMembers(item.Payload)...)
	raw := decodePushPayload(item.Payload)
	for field, kind := range map[string]string{"expense_id": "expenses", "cash_movement_id": "cash_movements", "recurring_occurrence_id": "expense_occurrences"} {
		if id, ok := raw[field].(string); ok && id != "" {
			keys = append(keys, expenseMemberKey(kind, id))
		}
	}
	return keys
}
func mergeExpenseGraphMetadata(existing, incoming json.RawMessage) json.RawMessage {
	keys := append(expenseGraphMembers(existing), expenseGraphMembers(incoming)...)
	if len(keys) == 0 {
		return incoming
	}
	raw := decodePushPayload(incoming)
	if raw == nil {
		return incoming
	}
	raw[expenseGraphMembersKey] = uniqueExpenseKeys(keys)
	encoded, err := json.Marshal(raw)
	if err != nil {
		return incoming
	}
	return encoded
}
func uniqueExpenseKeys(keys []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, key := range keys {
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Components are inferred for legacy queued snapshots as well as explicitly
// declared for new commands. Independent expenses stay independent.
func groupExpensePushItems(batch []PushItem) [][]int {
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
		for _, key := range expenseGraphKeys(item) {
			if owner, ok := owners[key]; ok {
				parent[find(i)] = find(owner)
			} else {
				owners[key] = i
			}
		}
	}
	groups := map[int][]int{}
	for i, item := range batch {
		if expenseSyncType(item.EntityType) {
			root := find(i)
			groups[root] = append(groups[root], i)
		}
	}
	out := make([][]int, 0, len(groups))
	for _, group := range groups {
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
