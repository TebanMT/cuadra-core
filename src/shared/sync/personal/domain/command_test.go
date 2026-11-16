package domain

import (
	"encoding/json"
	"testing"
)

func TestCapturedAtPreservesUTCPrecisionAndRejectsNormalization(t *testing.T) {
	for _, value := range []string{"0001-01-01T00:00:00Z", "2024-02-29T23:59:59.000Z", "9999-12-31T23:59:59.999Z"} {
		if _, err := ParseCapturedAt(value); err != nil {
			t.Fatal("valid capture rejected")
		}
	}
	for _, value := range []string{"", "0000-01-01T00:00:00Z", "2026-02-29T00:00:00Z", "2026-01-01T24:00:00Z", "2026-01-01T00:00:60Z", "2026-01-01T00:00:00.1Z", "2026-01-01T00:00:00.0000Z", "2026-01-01T00:00:00+00:00"} {
		if _, err := ParseCapturedAt(value); err == nil {
			t.Fatal("invalid capture accepted")
		}
	}
}

func TestBaseRejectsNullMixedDuplicateAndUnknownVariants(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"revision":null}`, `{"after_operation_id":null}`, `{"revision":"1","after_operation_id":null}`, `{"revision":null,"after_operation_id":"10000000-0000-4000-8000-000000000001"}`, `{"revision":"1","revision":"1"}`, `{"revision":"1","Revision":"1"}`, `{"Revision":"1"}`, `{"revision":1}`, `[]`} {
		var base Base
		if json.Unmarshal([]byte(raw), &base) == nil {
			t.Fatal("ambiguous Base accepted", raw)
		}
	}
	for _, raw := range []string{`{"revision":"1"}`, `{"after_operation_id":"10000000-0000-4000-8000-000000000001"}`} {
		var base Base
		if json.Unmarshal([]byte(raw), &base) != nil {
			t.Fatal("valid Base rejected")
		}
		encoded, err := json.Marshal(base)
		if err != nil || string(encoded) != raw {
			t.Fatal("Base changed command before hashing")
		}
	}
}
