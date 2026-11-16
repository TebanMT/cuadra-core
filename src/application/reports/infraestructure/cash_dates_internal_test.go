package infraestructure

import (
	"reflect"
	"testing"
)

func TestSplitBusinessDates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", []string{}},
		{"single", "2026-11-03", []string{"2026-11-03"}},
		{"sorted and deduplicated", "2026-11-09,2026-11-03,2026-11-09", []string{"2026-11-03", "2026-11-09"}},
		{"timestamps trimmed to date", "2026-11-03T00:00:00Z, 2026-11-04", []string{"2026-11-03", "2026-11-04"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitBusinessDates(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitBusinessDates(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}
