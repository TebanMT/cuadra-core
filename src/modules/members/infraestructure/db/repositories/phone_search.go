package repositories

import "strings"

// stripPhoneSeparators normaliza un término de búsqueda para machearlo
// contra el campo `phone` de la BD (que se guarda solo con dígitos y
// opcional `+` líder, ver Member.applyPhone). Quita espacios, guiones,
// paréntesis y puntos para que tipear "55 1234" matchee "5512345678" y
// "+52 55 1234" matchee "+525512345678".
//
// Si el resultado queda sin dígitos ni `+` (caso "abc"), regresa "" para
// señalar que no es un término de teléfono — el caller debe omitir el
// LIKE sobre phone para no generar matches falsos con `%%`.
func stripPhoneSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '-', '(', ')', '.', '\t':
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	hasDigit := false
	for _, r := range out {
		if r >= '0' && r <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit {
		return ""
	}
	return out
}
