// Package domain — bounded context `notifications`.
//
// Sesión 7. Owns templates + dispatch (WhatsApp/email/push). Invoked from
// other BCs (e.g. billing dispara comprobante por WhatsApp después de un
// Payment exitoso).
//
// TODO(humano): implementar UC-037 (conectar WhatsApp Business), UC-038-041.
// MVP: tabla notification_queue + worker que drena vía sender simple.
package domain
