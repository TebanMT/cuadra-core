//go:build server

package sync

import "testing"

func TestCursorRoundTrip(t *testing.T) {
	cases := []FullCursor{
		{},
		{TypeIndex: 0, EntityID: ""},
		{TypeIndex: 5, EntityID: "abc-123"},
	}
	for _, c := range cases {
		s := EncodeCursor(c)
		got, err := DecodeCursor(s)
		if err != nil {
			t.Fatalf("decode %q: %v", s, err)
		}
		if got.TypeIndex != c.TypeIndex || got.EntityID != c.EntityID {
			t.Errorf("round-trip mismatch: in=%+v out=%+v", c, got)
		}
	}
}

func TestDecodeCursorInvalid(t *testing.T) {
	if _, err := DecodeCursor("not-json"); err == nil {
		t.Error("expected error for invalid cursor")
	}
}

func TestExtractUpdatedAt(t *testing.T) {
	if got := extractUpdatedAt(map[string]any{"updated_at": float64(1714060000000)}); got.IsZero() {
		t.Error("epoch ms not parsed")
	}
	if got := extractUpdatedAt(map[string]any{"updated_at": "2026-04-25T10:00:00Z"}); got.IsZero() {
		t.Error("RFC3339 not parsed")
	}
	if got := extractUpdatedAt(map[string]any{}); !got.IsZero() {
		t.Error("missing key should yield zero")
	}
}
