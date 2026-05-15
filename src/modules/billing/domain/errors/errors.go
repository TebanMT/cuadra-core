// Package errors holds the sentinel errors of the billing BC. Spanish
// messages are intended for surfacing to operators / owners. UC mappings:
//
//	UC-018 RegisterMembershipPayment -> ErrAmountInvalid, ErrPaymentMethodMissing,
//	                                    ErrPaymentMethodInvalid, ErrConceptInvalid,
//	                                    ErrDiscountReasonRequired, ErrDiscountTooLarge,
//	                                    ErrPartialAmountInvalid
//	UC-019 SettlePendingBalance      -> ErrPaymentNotFound, ErrCannotSettleConcept,
//	                                    ErrSettlementExceedsBalance
//	UC-020 GenerateReceipt           -> ErrPaymentNotFound
//	UC-022 RefundPayment             -> ErrCannotRefundNonPayment, ErrAlreadyRefunded,
//	                                    ErrRefundReasonRequired
package errors

import "errors"

var (
	// Generic
	ErrPaymentNotFound = errors.New("pago no encontrado")
	ErrCrossGym        = errors.New("ese pago no pertenece a tu gimnasio")

	// Validation
	ErrAmountInvalid          = errors.New("el monto debe ser mayor a cero")
	ErrPaymentMethodMissing   = errors.New("falta el método de pago")
	ErrPaymentMethodInvalid   = errors.New("método de pago inválido")
	ErrConceptInvalid         = errors.New("concepto de pago inválido")
	ErrDiscountReasonRequired = errors.New("la razón del descuento es obligatoria")
	ErrDiscountTooLarge       = errors.New("el descuento no puede ser mayor que el subtotal")
	ErrPartialAmountInvalid   = errors.New("el monto parcial debe ser mayor a cero y menor que el total")
	ErrPaymentDateInvalid     = errors.New("la fecha del pago no es válida")
	ErrNotesTooLong           = errors.New("las notas son demasiado largas")

	// Settlement (UC-019)
	ErrCannotSettleConcept      = errors.New("solo se puede liquidar saldo de pagos de membresía o producto")
	ErrSettlementExceedsBalance = errors.New("el abono excede el saldo pendiente")
	ErrNoBalancePending         = errors.New("este pago no tiene saldo pendiente")

	// Refund (UC-022)
	ErrCannotRefundNonPayment = errors.New("solo se pueden cancelar pagos originales (no settlements ni refunds)")
	ErrAlreadyRefunded        = errors.New("este pago ya fue cancelado previamente")
	ErrRefundReasonRequired   = errors.New("la razón de la cancelación es obligatoria")
	ErrRefundOnlyOwner        = errors.New("solo el dueño puede cancelar pagos")

	// Sale (UC-025/UC-026)
	ErrSaleEmpty               = errors.New("el carrito de venta está vacío")
	ErrSaleItemNameRequired    = errors.New("falta el nombre del producto en la línea de venta")
	ErrSaleItemPriceInvalid    = errors.New("el precio del producto en la venta debe ser mayor a cero")
	ErrSaleItemQuantityInvalid = errors.New("la cantidad de la línea de venta debe ser mayor a cero")
	ErrSaleNotFound            = errors.New("venta no encontrada")
	// Fiado: dejar saldo pendiente en una venta exige un socio asociado.
	// No se fía a un walk-in porque no habría a quién cobrar después.
	ErrCreditRequiresMember = errors.New("para dejar saldo pendiente debes asociar un socio")
)
