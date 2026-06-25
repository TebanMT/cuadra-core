// Package phone centraliza la normalización de teléfonos a E.164. Es la ÚNICA
// fuente de verdad del formato: todo lo que se almacena o se manda a Twilio
// (WhatsApp) debe pasar por aquí para que nunca lleguen espacios, guiones,
// paréntesis ni números sin código de país (que Twilio interpreta mal).
//
// Reemplaza a las normalizaciones divergentes que vivían dispersas
// (member.normalizePhone, user.NormalizePhone, import_csv.sanitizePhoneRaw,
// whatsappAddress). El paso CLAVE que faltaba: anteponer el código de país a
// los números nacionales de 10 dígitos — sin él, "4461057446" se mandaba como
// "+4461057446" y Twilio lo leía con un país equivocado.
package phone

import (
	"regexp"
	"strings"
)

// DefaultCountryCode es el código de país asumido cuando el número llega como
// 10 dígitos nacionales sin '+' ni código. México (+52). El FE manda el código
// de país explícito vía el selector de PhoneInput; este default sólo cubre
// entradas crudas (import legacy, llamadas directas al API, datos viejos).
const DefaultCountryCode = "52"

// e164Regex valida un teléfono YA normalizado: '+' obligatorio, primer dígito
// 1-9, total 10-15 dígitos. Más estricto que el viejo `^\+?[1-9]\d{9,14}$`
// (que dejaba pasar 10 dígitos pelados sin '+').
var e164Regex = regexp.MustCompile(`^\+[1-9]\d{9,14}$`)

// Normalize limpia un teléfono crudo y lo regresa en E.164 (`+<cc><nacional>`),
// sin separadores. Reglas:
//   - Se queda sólo con los dígitos (y recuerda si venía un '+' líder).
//   - Si NO traía '+' y quedan exactamente 10 dígitos → antepone el código de
//     país por default (MX → "+52"). Este es el paso que evita el bug de país.
//   - Si NO traía '+' y quedan 11+ dígitos → asume que ya incluye el código de
//     país y sólo antepone '+'.
//   - Si traía '+', respeta el código de país existente.
//
// Devuelve "" si el input queda sin dígitos (campo vacío/opcional) — el caller
// decide si eso es NULL o error.
func Normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	hadPlus := strings.HasPrefix(raw, "+")
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if digits == "" {
		return ""
	}
	if !hadPlus && len(digits) == 10 {
		return "+" + DefaultCountryCode + digits
	}
	return "+" + digits
}

// Valid reporta si un teléfono YA normalizado (salida de Normalize) cumple
// E.164 estricto. Útil para validar en los use cases tras normalizar.
func Valid(e164 string) bool {
	return e164Regex.MatchString(e164)
}

// NormalizeValid normaliza y valida en un paso. ok=false cuando el resultado
// no es E.164 (p.ej. demasiado corto). El string devuelto es siempre el
// normalizado (aunque ok sea false) para logging.
func NormalizeValid(raw string) (normalized string, ok bool) {
	n := Normalize(raw)
	return n, Valid(n)
}
