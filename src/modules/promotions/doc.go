// Package promotions implementa el BC de promociones (descuentos,
// cupones, 2x1 y días gratis) — feature del plan Standard.
//
// Estructura:
//
//	app/             Use cases (CreatePromotion, ApplyPromotion, …)
//	domain/          Aggregate Promotion + entity AppliedPromotion +
//	                 Calculator puro + sentinels + repository interfaces
//	infraestructure/ Modelos GORM + repositorios Postgres / SQLite
//	interfaces/      Controllers HTTP (owner-only en mutaciones)
//
// Una promo aplica a un cobro (membresía o venta de producto). El cobro
// guarda el descuento en `payments.discount_amount`; el `applied_promotions`
// registra la aplicación con snapshot inmutable de la promo al momento
// del cobro, permitiendo editar/desactivar la promo después sin afectar
// cobros viejos.
//
// MVP: máximo 1 promo por cobro (sin stacking). Buy_n=1 fijo en el form
// (campo en schema para futuro N>1).
package promotions
