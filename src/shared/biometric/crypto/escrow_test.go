package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestParseSMK(t *testing.T) {
	// Vacía = escrow deshabilitado, sin error (fail-closed río abajo).
	if smk, err := ParseSMK(""); err != nil || smk != nil {
		t.Fatalf("vacía debe ser (nil, nil), got (%v, %v)", smk, err)
	}
	if smk, err := ParseSMK("   "); err != nil || smk != nil {
		t.Fatalf("espacios deben ser (nil, nil), got (%v, %v)", smk, err)
	}
	if _, err := ParseSMK("no-es-base64!!"); err == nil {
		t.Errorf("base64 inválido debe fallar")
	}
	if _, err := ParseSMK(base64.StdEncoding.EncodeToString([]byte("corta"))); err == nil {
		t.Errorf("longitud != 32 debe fallar")
	}
	raw := make([]byte, GMKSize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	smk, err := ParseSMK(base64.StdEncoding.EncodeToString(raw))
	if err != nil || len(smk) != GMKSize {
		t.Fatalf("SMK válida rechazada: %v", err)
	}
}

func TestWrapUnwrapGMK_RoundTrip(t *testing.T) {
	smk := make([]byte, GMKSize)
	gmk := make([]byte, GMKSize)
	rand.Read(smk)
	rand.Read(gmk)

	blob, err := WrapGMK(smk, gmk)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got, err := UnwrapGMK(smk, blob)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if !bytesEq(got, gmk) {
		t.Errorf("round-trip no preservó la GMK")
	}

	// SMK equivocada = no descifra (GCM autentica).
	otra := make([]byte, GMKSize)
	rand.Read(otra)
	if _, err := UnwrapGMK(otra, blob); err == nil {
		t.Errorf("unwrap con SMK equivocada debe fallar")
	}

	// GMK de tamaño inválido no se wrappea.
	if _, err := WrapGMK(smk, []byte("corta")); err == nil {
		t.Errorf("wrap de GMK corta debe fallar")
	}
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
