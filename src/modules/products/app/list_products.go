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
	GymID           uuid.UUID
	Search          string
	Category        string
	IncludeInactive bool
	LowStockOnly    bool
	Page            int
	PageSize        int
}

type ListProductsOutput struct {
	Items    []*productDomain.Product
	Total    int
	Page     int
	PageSize int
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
	rows, total, err := uc.Products.List(tx, prodRepo.ListQuery{
		GymID:           in.GymID,
		Search:          in.Search,
		Category:        in.Category,
		IncludeInactive: in.IncludeInactive,
		LowStockOnly:    in.LowStockOnly,
		Page:            page,
		PageSize:        pageSize,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &ListProductsOutput{Items: rows, Total: total, Page: page, PageSize: pageSize}, nil
}
