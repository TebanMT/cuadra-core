//go:build server

package sync

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestOrderMembershipReplacements(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targets []string
		want    []int
	}{
		{"renewal", []string{"new", ""}, []int{0, 2, 1, 3}},
		{"already ordered", []string{"", "old"}, []int{0, 1, 2, 3}},
		{"missing target remains for database validation", []string{"missing", ""}, []int{0, 1, 2, 3}},
		{"cycle terminates without dropping rows", []string{"new", "old"}, []int{0, 2, 1, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch := []PushItem{{EntityType: "members"}, {EntityType: "memberships", EntityID: "old"}, {EntityType: "memberships", EntityID: "new"}, {EntityType: "payments"}}
			for i, target := range tc.targets {
				batch[i+1].Payload, _ = json.Marshal(map[string]any{"replaced_by": target})
			}
			got := orderMembershipReplacements(batch, []int{0, 1, 2, 3})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
