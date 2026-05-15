// Package errors holds the sentinel errors of the expenses BC. Spanish
// messages are intended for surfacing to the gym owner.
package errors

import "errors"

var (
	// Generic
	ErrExpenseNotFound = errors.New("gasto no encontrado")
	ErrCrossGym        = errors.New("ese gasto no pertenece a tu gimnasio")

	// Validation
	ErrInvalidAmount        = errors.New("el monto debe ser mayor a cero")
	ErrInvalidCategory      = errors.New("categoría de gasto inválida")
	ErrInvalidPaymentMethod = errors.New("método de pago inválido")
	ErrInvalidDescription   = errors.New("la descripción no debe pasar de 200 caracteres")
	ErrInvalidDate          = errors.New("la fecha del gasto es inválida")
)
