package app

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Older clients retain their original fingerprint after the API upgrade.
// An absent optional confirmation must not introduce a new JSON null field.
func TestSaleFingerprintOmittedConfirmationKeepsLegacyWire(t *testing.T) {
	input := RegisterSaleInput{}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("ExpectedTotal")) {
		t.Fatal("new optional field changed legacy command fingerprint")
	}
	expected := 0.0
	input.ExpectedTotal = &expected
	raw, err = json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"ExpectedTotal":0`)) {
		t.Fatal("explicit zero confirmation was omitted")
	}
}
