package phone

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Espacios / guiones / paréntesis / puntos → se quitan.
		{"+52 446 105 7446", "+524461057446"},
		{"+52-446-105-7446", "+524461057446"},
		{"(+52) 446 105 7446", "+524461057446"},
		{"+52.446.105.7446", "+524461057446"},
		{"  +524461057446  ", "+524461057446"},
		// 10 dígitos nacionales sin '+' → antepone +52 (el paso clave).
		{"4461057446", "+524461057446"},
		{"446 105 7446", "+524461057446"},
		// Ya trae código de país sin '+' (12 dígitos) → sólo antepone '+'.
		{"524461057446", "+524461057446"},
		// Frontera: 11+ dígitos sin '+' se asumen CON código de país (no se
		// les mete +52). Sólo los de exactamente 10 reciben el default MX.
		{"15551234567", "+15551234567"},       // 11 díg → +1 (USA), no +52
		{"5215551234567", "+5215551234567"},   // 13 díg → respeta tal cual
		// Otro país con '+' explícito → respeta su código.
		{"+1 415 555 1234", "+14155551234"},
		{"+14155551234", "+14155551234"},
		// Vacío / sin dígitos.
		{"", ""},
		{"   ", ""},
		{"sin numero", ""},
		// Placeholder del import ("—") → sin dígitos → "".
		{"—", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValid(t *testing.T) {
	valid := []string{"+524461057446", "+14155551234", "+5212345678901"}
	for _, v := range valid {
		if !Valid(v) {
			t.Errorf("Valid(%q) = false, want true", v)
		}
	}
	invalid := []string{
		"4461057446",        // sin '+'
		"+52 446 105 7446",  // con espacios
		"+0446105744",       // empieza con 0
		"+52446",            // muy corto
		"",                  // vacío
	}
	for _, v := range invalid {
		if Valid(v) {
			t.Errorf("Valid(%q) = true, want false", v)
		}
	}
}

func TestNormalizeThenValid(t *testing.T) {
	// La salida de Normalize SIEMPRE debe pasar Valid cuando hay ≥10 dígitos.
	for _, raw := range []string{"+52 446 105 7446", "4461057446", "524461057446", "+1 415 555 1234"} {
		n, ok := NormalizeValid(raw)
		if !ok {
			t.Errorf("NormalizeValid(%q) = (%q, false), want valid", raw, n)
		}
	}
}
