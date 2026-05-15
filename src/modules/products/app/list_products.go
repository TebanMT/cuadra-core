package app

import (
	"context"

	"github.com/google/uuid"

	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListProductsInput backs UC-023 (listado). The recepción UI calls this every
// time the venta grid opens, so paging defaults are friendly.
type ListProductsInput struct {
	GymID        uuid.UUID
	Search       string
	Category     string
	ActiveFilter string // "active" | "inactive" | "all"; empty defaults to "active".
	LowStockOnly bool
	Sort         string // prodRepo.Sort* — empty defaults a "name".
	Direction    string // prodRepo.SortDir* — empty defaults a "asc".
	Page         int
	PageSize     int
}

type ListProductsOutput struct {
	Items      []*productDomain.Product
	Total      int
	Page       int
	PageSize   int
	Aggregates prodRepo.ProductAggregates // stats globales del filtro completo
}

type ListProducts struct {
	Products prodRepo.ProductRepository
	UoW      sharedDomain.UnitOfWork
}

func NewListProducts(products prodRepo.ProductRepository, uow sharedDomain.UnitOfWork) *ListProducts {
	return &ListProducts{Products: products, UoW: uow}
}

func (uc *ListProducts) Execute(ctx context.Context, in ListProductsInput) (*ListProductsOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	page := in.Page
	if page < 1 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	listQuery := prodRepo.ListQuery{
		GymID:        in.GymID,
		Search:       in.Search,
		Category:     in.Category,
		ActiveFilter: in.ActiveFilter,
		LowStockOnly: in.LowStockOnly,
		Sort:         in.Sort,
		Direction:    in.Direction,
		Page:         page,
		PageSize:     pageSize,
	}
	rows, total, err := uc.Products.List(tx, listQuery)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	// Aggregates corre sobre el mismo filtro (sin paginar) — alimenta
	// las StatCards del FE. Si falla, degrada a ceros: prefiero que la
	// página renderee con stats vacías que romperla porque un SUM falló.
	aggs, _ := uc.Products.ListAggregates(tx, listQuery)
	return &ListProductsOutput{
		Items:      rows,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		Aggregates: aggs,
	}, nil
}
