// Package errors holds the sentinel errors of the expenses BC. Spanish
// messages are intended for surfacing to the gym owner.
package errors

import "errors"

var (
	// Generic
	ErrExpenseNotFound = errors.New("gasto no encontrado")
	ErrCrossGym        = errors.New("ese gasto no pertenece a tu gimnasio")

	// Validation
	ErrInvalidAmount               = errors.New("el monto debe ser mayor a cero")
	ErrInvalidCategory             = errors.New("categoría de gasto inválida")
	ErrInvalidPaymentMethod        = errors.New("método de pago inválido")
	ErrInvalidPaidFrom             = errors.New("origen del dinero inválido")
	ErrInvalidDescription          = errors.New("la descripción no debe pasar de 200 caracteres")
	ErrInvalidDate                 = errors.New("la fecha del gasto es inválida")
	ErrFutureDate                  = errors.New("la fecha de pago no puede estar en el futuro")
	ErrInvalidPayee                = errors.New("el beneficiario no debe pasar de 120 caracteres")
	ErrInvalidReference            = errors.New("la referencia no debe pasar de 120 caracteres")
	ErrInvalidClassification       = errors.New("la clasificación debe ser fija o variable")
	ErrInvalidSource               = errors.New("el origen del gasto es inválido")
	ErrDayClosed                   = errors.New("ese día ya fue cerrado; registra un ajuste nuevo en el día abierto")
	ErrVersionConflict             = errors.New("alguien más modificó este registro; actualiza la pantalla e inténtalo de nuevo")
	ErrCashMovementNotFound        = errors.New("movimiento de caja no encontrado")
	ErrInvalidCashMovementStatus   = errors.New("estado de movimiento de caja inválido")
	ErrInvalidCashMovementPurpose  = errors.New("elige si la salida es para pagar un gasto o entregar dinero")
	ErrTemplateNotFound            = errors.New("gasto recurrente no encontrado")
	ErrOccurrenceNotFound          = errors.New("vencimiento no encontrado")
	ErrInvalidOccurrenceStatus     = errors.New("estado de vencimiento inválido")
	ErrInvalidPagination           = errors.New("paginación inválida")
	ErrAlreadyClassified           = errors.New("esta salida ya fue clasificada")
	ErrCashInCannotBeClassified    = errors.New("una entrada física no puede convertirse en gasto o compra; registra un ingreso extraordinario si corresponde")
	ErrLinkedExpense               = errors.New("este gasto conserva un vínculo de origen y no puede eliminarse; registra un ajuste")
	ErrIdempotencyKeyInvalid       = errors.New("idempotency_key es requerido y no debe pasar de 120 caracteres")
	ErrIdempotencyKeyConflict      = errors.New("idempotency_key ya fue usado con otros datos")
	ErrTemplateScheduleHasPending  = errors.New("esta regla tiene vencimientos pendientes; págales u omítelos antes de cambiar sus fechas o frecuencia")
	ErrTemplateIdempotencyConflict = errors.New("esa clave de idempotencia ya se usó para otra regla recurrente")
	ErrLinkedCashMovement          = errors.New("este movimiento ya está clasificado y debe corregirse desde su registro de origen")
	ErrCashMovementNotCompatible   = errors.New("la salida de caja no coincide con la fecha, monto o caja de esta compra")
	ErrCorrectionReasonRequired    = errors.New("indica un motivo de corrección de al menos 3 caracteres")
	ErrCorrectionOwnerRequired     = errors.New("sólo el propietario puede deshacer una clasificación o vencimiento resuelto")
)
