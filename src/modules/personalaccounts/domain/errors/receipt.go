package errors

import "errors"

var (
	ErrInvalidReceipt      = errors.New("recibo de acceso inválido")
	ErrReceiptNotFound     = errors.New("resultado no encontrado")
	ErrIdempotencyMismatch = errors.New("el identificador ya corresponde a otra petición")
	ErrResultExpired       = errors.New("el resultado protegido ha vencido")
)
