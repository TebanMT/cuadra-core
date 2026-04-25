// Package domain — bounded context `billing`.
//
// Sesión 3 + parts of Sesión 4. Owns Payment + Sale aggregates; orchestrates
// (NOT owns) consequences in members and products inside the same UoW
// (see CUADRA-SPEC.md §6.4).
//
// TODO(humano): implementar UC-018..UC-022 (cobros, comprobantes, refunds),
// UC-025..UC-026 (venta de productos, devolución).
package domain
