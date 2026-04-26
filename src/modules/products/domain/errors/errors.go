// Package errors holds the sentinel errors of the products BC. Spanish
// messages are intended for surfacing to operators / owners. UC mappings:
//
//	UC-023 Create/Update/Deactivate -> ErrInvalidName, ErrInvalidPrice, ErrInvalidStock,
//	                                   ErrNameAlreadyExists, ErrProductNotFound, ErrProductInactive
//	UC-024 AdjustStock              -> ErrInvalidAdjustment, ErrInsufficientStock,
//	                                   ErrInvalidMovementType
//	UC-025 RegisterSale (cross-BC)  -> ErrInsufficientStock, ErrProductNotFound,
//	                                   ErrProductInactive, ErrCrossGym
package errors

import "errors"

var (
	// Generic
	ErrProductNotFound = errors.New("producto no encontrado")
	ErrCrossGym        = errors.New("ese producto no pertenece a tu gimnasio")
	ErrProductInactive = errors.New("producto inactivo")

	// Validation
	ErrInvalidName       = errors.New("el nombre del producto debe tener entre 2 y 100 caracteres")
	ErrInvalidPrice      = errors.New("el precio debe ser mayor a cero")
	ErrInvalidStock      = errors.New("el stock no puede ser negativo")
	ErrInvalidStockMin   = errors.New("el stock mínimo no puede ser negativo")
	ErrNameAlreadyExists = errors.New("ya existe un producto con ese nombre en este gimnasio")

	// Stock movement (UC-024)
	ErrInvalidAdjustment   = errors.New("la cantidad del ajuste debe ser mayor a cero")
	ErrInvalidMovementType = errors.New("tipo de ajuste de stock inválido")
	ErrAdjustmentNoChange  = errors.New("el ajuste no cambia el stock")

	// Sale path (UC-025)
	ErrInsufficientStock = errors.New("stock insuficiente para esta venta")
)
