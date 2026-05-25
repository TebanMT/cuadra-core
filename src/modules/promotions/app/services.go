// Package app implementa los use cases del BC promotions.
//
//	CreatePromotion / UpdatePromotion / Deactivate / Reactivate
//	ListPromotions  / GetPromotionByCode
//	ApplyPromotion  — el hot-path desde billing
//	ListAppliedByMonth — para el reporte del dashboard
//
// Las mutaciones (Create/Update/Deactivate/Reactivate) usan UoW.Command
// y registran audit. ApplyPromotion no abre UoW propia — toma la tx
// que le pasa el use case de billing y opera dentro de ella; si algo
// falla el rollback de billing cubre todo.
package app
