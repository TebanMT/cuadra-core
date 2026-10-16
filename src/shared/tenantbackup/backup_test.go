//go:build server

package tenantbackup

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestArchiveAuthentication(t *testing.T) {
	key := bytes.Repeat([]byte{19}, 32)
	a := archive{Format: 1, Gym: uuid.New(), Tables: []table{{Name: "users", Rows: json.RawMessage(`[{"password_hash":"private-synthetic-value"}]`)}}}
	raw, _ := json.Marshal(a)
	data, err := seal(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private-synthetic-value")) {
		t.Fatal("plaintext leaked")
	}
	other, _ := seal(raw, key)
	if bytes.Equal(data, other) {
		t.Fatal("nonce reused")
	}
	got, err := open(data, key)
	if err != nil || got.Gym != a.Gym {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := open(data, bytes.Repeat([]byte{20}, 32)); err == nil {
		t.Fatal("wrong key accepted")
	}
	for _, offset := range []int{0, 6, len(data) - 1} {
		corrupted := append([]byte(nil), data...)
		corrupted[offset] ^= 1
		if _, err := open(corrupted, key); err == nil {
			t.Fatalf("tamper accepted at %d", offset)
		}
	}
	if _, err := open(data[:8], key); err == nil {
		t.Fatal("truncation accepted")
	}
}
