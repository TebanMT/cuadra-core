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
	ErrInvalidAdjustment             = errors.New("la cantidad del ajuste debe ser mayor a cero")
	ErrInvalidMovementType           = errors.New("tipo de ajuste de stock inválido")
	ErrAdjustmentNoChange            = errors.New("el ajuste no cambia el stock")
	ErrInvalidPurchase               = errors.New("la compra de inventario es inválida")
	ErrAdjustmentIdempotencyRequired = errors.New("el ajuste de inventario requiere una clave de idempotencia")
	ErrAdjustmentIdempotencyConflict = errors.New("esa clave de idempotencia ya se usó para otro ajuste de inventario")
	ErrInvalidPurchaseCost           = errors.New("el costo unitario de la compra debe ser positivo y tener centavos válidos")
	ErrInvalidPurchaseMethod         = errors.New("el método de pago de la compra es inválido")
	ErrInvalidPurchaseSource         = errors.New("el origen del dinero de la compra es inválido")
	ErrPurchaseDateInvalid           = errors.New("la fecha de pago de la compra no es válida")
	ErrIncompletePurchasePayment     = errors.New("completa costo, fecha, método y origen del pago de la compra")
	ErrPurchaseIdempotencyRequired   = errors.New("la compra requiere una clave de idempotencia")
	ErrPurchaseIdempotencyConflict   = errors.New("esa clave de idempotencia ya se usó para otra compra")
	ErrPurchaseNotFound              = errors.New("compra de inventario no encontrada")
	ErrPurchaseVersionConflict       = errors.New("la compra cambió en otro dispositivo; actualiza e intenta de nuevo")
	ErrPurchaseAlreadyResolved       = errors.New("la compra ya fue pagada con datos distintos")
	ErrPurchaseOwnerRequired         = errors.New("sólo el propietario puede pagar o corregir compras pendientes")
	ErrPurchaseCorrectionReason      = errors.New("indica un motivo de corrección de al menos 3 caracteres")
	ErrPurchaseMustBeReopened        = errors.New("reabre el pago antes de corregir o anular esta compra")
	ErrPurchaseCorrectionNoChange    = errors.New("la corrección no cambia cantidad ni costo")
	ErrPurchaseCorrectionConflict    = errors.New("esa clave de idempotencia ya se usó para otra corrección de compra")

	// Sale path (UC-025)
	ErrInsufficientStock = errors.New("stock insuficiente para esta venta")
)

var ErrRemotePurchaseCloudOnly = errors.New("este pago se administra desde la web del dueño")
var ErrPurchaseReceiptConflict = errors.New("esta compra ya tiene una recepción diferente; revisa las unidades recibidas")
var ErrPurchaseReceiptInvalid = errors.New("no se puede recibir esta compra; revisa el producto y las unidades")
