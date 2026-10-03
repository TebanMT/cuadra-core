package sync

import (
	"encoding/json"
	"sort"
	"strings"
)

const (
	refundGraphIDsKey           = "_sync_graph_ids"
	refundGraphExpectedItemsKey = "_sync_graph_expected_items"
)

func refundGraphIDs(payload json.RawMessage) []string {
	raw := decodePushPayload(payload)
	value, ok := raw[refundGraphIDsKey]
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	appendID := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, duplicate := seen[value]; duplicate {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	switch values := value.(type) {
	case []any:
		for _, entry := range values {
			if id, ok := entry.(string); ok {
				appendID(id)
			}
		}
	case []string:
		for _, id := range values {
			appendID(id)
		}
	case string:
		appendID(values)
	}
	sort.Strings(out)
	return out
}

func decodePushPayload(payload json.RawMessage) map[string]any {
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	return raw
}
