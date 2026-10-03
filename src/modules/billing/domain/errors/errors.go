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
	ErrAmountInvalid                  = errors.New("el monto debe ser mayor a cero")
	ErrPaymentMethodMissing           = errors.New("falta el método de pago")
	ErrPaymentMethodInvalid           = errors.New("método de pago inválido")
	ErrConceptInvalid                 = errors.New("concepto de pago inválido")
	ErrDiscountReasonRequired         = errors.New("la razón del descuento es obligatoria")
	ErrDiscountTooLarge               = errors.New("el descuento no puede ser mayor que el subtotal")
	ErrPartialAmountInvalid           = errors.New("el monto parcial debe ser mayor a cero y menor que el total")
	ErrPaymentDateInvalid             = errors.New("la fecha del pago no es válida")
	ErrNotesTooLong                   = errors.New("las notas son demasiado largas")
	ErrOtherIncomeDescriptionRequired = errors.New("describe de dónde proviene el ingreso")
	ErrIdempotencyKeyRequired         = errors.New("la operación requiere una clave de idempotencia")
	ErrIdempotencyKeyConflict         = errors.New("esa clave de idempotencia ya se usó con datos distintos")

	// Settlement (UC-019)
	ErrCannotSettleConcept      = errors.New("solo se puede liquidar saldo de pagos de membresía o producto")
	ErrSettlementExceedsBalance = errors.New("el abono excede el saldo pendiente")
	ErrNoBalancePending         = errors.New("este pago no tiene saldo pendiente")

	// Refund (UC-022)
	ErrCannotRefundNonPayment    = errors.New("solo se pueden devolver cobros originales o abonos; una devolución no se puede devolver")
	ErrAlreadyRefunded           = errors.New("este pago ya fue cancelado previamente")
	ErrRefundReasonRequired      = errors.New("la razón de la cancelación es obligatoria")
	ErrRefundOnlyOwner           = errors.New("solo el dueño puede cancelar pagos")
	ErrRefundExceedsCollected    = errors.New("la devolución excede lo efectivamente cobrado y aún no devuelto")
	ErrRefundIdempotencyRequired = errors.New("la devolución requiere una clave de idempotencia")
	ErrRefundIdempotencyConflict = errors.New("esa clave de idempotencia ya se usó para otra devolución")
	ErrRefundDispositionInvalid  = errors.New("indica si el producto volvió a stock, se dañó o no fue devuelto")
	ErrRefundQuantityExceeded    = errors.New("la cantidad devuelta excede las unidades vendidas disponibles")
	ErrMembershipRevertUnsafe    = errors.New("esta membresía no puede cancelarse automáticamente porque el pago no conserva todos los efectos originales; registra sólo la devolución y ajusta la membresía por separado")

	// Sale (UC-025/UC-026)
	ErrSalePaidInvalid                 = errors.New("el abono debe estar entre cero y el total de la venta")
	ErrSaleTotalChanged                = errors.New("cambió el total de la venta; revisa los precios antes de cobrar")
	ErrSaleEmpty                       = errors.New("el carrito de venta está vacío")
	ErrSaleItemNameRequired            = errors.New("falta el nombre del producto en la línea de venta")
	ErrSaleItemPriceInvalid            = errors.New("el precio del producto en la venta debe ser mayor a cero")
	ErrSaleItemQuantityInvalid         = errors.New("la cantidad de la línea de venta debe ser mayor a cero")
	ErrSaleNotFound                    = errors.New("venta no encontrada")
	ErrSaleVersionConflict             = errors.New("la venta cambió; actualiza la pantalla e inténtalo de nuevo")
	ErrSaleCorrectionReasonRequired    = errors.New("explica por qué corriges la venta")
	ErrSaleCorrectionForbidden         = errors.New("esta corrección requiere autorización del dueño")
	ErrSaleCorrectionResolutionInvalid = errors.New("elige cómo resolver la diferencia cobrada")
	ErrSaleCorrectionSnapshotInvalid   = errors.New("no se pudo construir la evidencia de la corrección")
	ErrSaleCorrectionUnsupported       = errors.New("por ahora sólo se pueden corregir ventas pagadas completamente y sin abonos")
	ErrSaleCorrectionNotFound          = errors.New("corrección de venta no encontrada")
	ErrPendingRefundNotFound           = errors.New("no hay un monto pendiente por devolver en esta corrección")
	ErrPendingRefundExceedsDue         = errors.New("la devolución excede el monto pendiente por devolver")
	// Fiado: dejar saldo pendiente en una venta exige un socio asociado.
	// No se fía a un walk-in porque no habría a quién cobrar después.
	ErrCreditRequiresMember = errors.New("para dejar saldo pendiente debes asociar un socio")

	// Administrative payment correction (owner-only). Product payments use
	// the sale correction aggregate because inventory must move atomically.
	ErrPaymentCorrectionReasonRequired  = errors.New("explica por qué corriges el pago")
	ErrPaymentCorrectionForbidden       = errors.New("solo el dueño puede corregir este pago")
	ErrPaymentCorrectionUnsupported     = errors.New("este pago requiere su flujo específico de venta, abono o devolución")
	ErrPaymentCorrectionAnnulMembership = errors.New("no puedes anular aquí un pago de membresía porque cambiaría la vigencia; usa el flujo de cancelación del servicio")
	ErrPaymentCorrectionAnnulMixed      = errors.New("una anulación no puede mezclar cambios de monto, método, caja o fecha")
	ErrPaymentCorrectionNoChanges       = errors.New("la corrección no cambia ningún dato del pago")
	ErrPaymentCorrectionNotFound        = errors.New("corrección de pago no encontrada")
	ErrPaymentVersionConflict           = errors.New("el pago cambió; actualiza la pantalla e inténtalo de nuevo")

	// Cash close (UC-027)
	ErrCashCloseAlreadyExists = errors.New("la caja de este día ya fue cerrada y no se puede reemplazar")
	ErrCashCloseNotFound      = errors.New("no hay un corte cerrado para reabrir en este día")
	ErrCashCloseConflict      = errors.New("el corte cambió; actualiza la pantalla e inténtalo de nuevo")
	ErrCashCloseFutureDate    = errors.New("no puedes cerrar una caja con fecha futura")
)
