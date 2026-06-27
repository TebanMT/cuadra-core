package app

import (
	"strings"
	"time"
)

// staleTTL devuelve cuánto puede esperar un mensaje —desde que OCURRIÓ el
// evento (Notification.CreatedAt)— antes de que enviarlo tarde deje de tener
// sentido. Si la antigüedad supera el TTL, el dispatcher lo retiene (`held`)
// en vez de mandarlo: no se pierde, pero no se envía 1-2 semanas tarde.
//
// Fase 1 de la supresión de mensajes stale (ver project_backlog). Los TTL son
// GENEROSOS a propósito: un outage normal de fin de semana SÍ debe enviar; sólo
// lo genuinamente viejo (1-2 semanas, el caso que motivó esto) se retiene.
//
// El ancla es CreatedAt, la hora real del evento (se genera client-side offline
// y se preserva por el sync), no la hora de llegada al cloud. Los recordatorios
// de vencimiento NO trip-ean esto: los genera el scheduler del cloud con
// CreatedAt≈ahora, así que su antigüedad al despachar siempre es chica.
//
// TTL == 0 significa "nunca stale" (no retener).
func staleTTL(templateKey string) time.Duration {
	const day = 24 * time.Hour
	switch {
	case strings.HasPrefix(templateKey, "owner_alert_"):
		// Alertas operativas (corte de caja, sin pagos hoy, stock bajo): a 2
		// días ya son ruido — describen un estado de ayer.
		return 2 * day
	case strings.HasPrefix(templateKey, "receipt_"):
		// NO son recibos timbrados (CFDI): un comprobante de un pago de ayer no
		// aporta nada. Si no salió el mismo día, mejor retenerlo.
		return 1 * day
	case templateKey == "member_welcome_number",
		templateKey == "operator_welcome_pin",
		templateKey == "owner_welcome":
		// 1 día. La bienvenida lleva credencial (número de socio / PIN de login
		// del operador) que NO expira; 2h era muy agresivo para el PIN del
		// operador, que es credencial de acceso y puede necesitarse más tarde.
		// 1d da margen sin caer en el "¡bienvenido!" 1-2 semanas tarde.
		return 1 * day
	case isMarketingTemplate(templateKey):
		// Broadcast / promos / avisos: time-sensitive. Un "mañana cerramos" o
		// una promo de 3 días no sirve días después → mismo trato que el recibo
		// (1 día). Atado por CATEGORÍA (Marketing), no por key, para que
		// cualquier template de marketing futuro herede el TTL corto.
		return 1 * day
	default:
		// Resto (transaccional / auth no listado arriba): umbral amplio.
		return 14 * day
	}
}

// isStale reporta si la notificación ya pasó su TTL de frescura a la hora `now`.
// TTL 0 → nunca stale.
func isStale(templateKey string, createdAt, now time.Time) bool {
	ttl := staleTTL(templateKey)
	return ttl > 0 && now.Sub(createdAt) > ttl
}
