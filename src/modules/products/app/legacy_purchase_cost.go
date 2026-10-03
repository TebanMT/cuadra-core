package app

import (
	"context"
	"errors"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchaseDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

type ListMissingPurchaseCosts struct {
	Rows prodRepo.LegacyPurchaseCostRepository
	Gyms gymRepo.GymRepository
	UoW  sharedDomain.UnitOfWork
}

func NewListMissingPurchaseCosts(rows prodRepo.LegacyPurchaseCostRepository, gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork) *ListMissingPurchaseCosts {
	return &ListMissingPurchaseCosts{rows, gyms, uow}
}

type MissingPurchaseCostsInput struct {
	GymID    uuid.UUID
	From, To time.Time
	Page     int
}
type MissingPurchaseCostsOutput struct {
	Items    []prodRepo.LegacyPurchaseCost `json:"items"`
	Total    int                           `json:"total"`
	Page     int                           `json:"page"`
	PageSize int                           `json:"page_size"`
}

func (uc *ListMissingPurchaseCosts) Execute(ctx context.Context, in MissingPurchaseCostsInput) (*MissingPurchaseCostsOutput, error) {
	if in.From.IsZero() || in.To.IsZero() || in.From.After(in.To) {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchase)
	}
	if in.Page < 1 {
		in.Page = 1
	}
	out := &MissingPurchaseCostsOutput{Items: []prodRepo.LegacyPurchaseCost{}, Page: in.Page, PageSize: 50}
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		loc, err := purchaseLocation(tx, uc.Gyms, in.GymID)
		if err != nil {
			return err
		}
		from := time.Date(in.From.Year(), in.From.Month(), in.From.Day(), 0, 0, 0, 0, loc).UTC()
		to := time.Date(in.To.Year(), in.To.Month(), in.To.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1).UTC()
		out.Items, out.Total, err = uc.Rows.ListMissingPurchaseCosts(tx, in.GymID, from, to, out.Page, out.PageSize)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		for i := range out.Items {
			out.Items[i].RecordedOn = out.Items[i].OccurredAt.In(loc).Format("2006-01-02")
		}
		return err
	})
	return out, err
}

type CompleteLegacyPurchaseCost struct {
	Rows      prodRepo.LegacyPurchaseCostRepository
	Purchases prodRepo.InventoryPurchaseRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
}

func NewCompleteLegacyPurchaseCost(rows prodRepo.LegacyPurchaseCostRepository, purchases prodRepo.InventoryPurchaseRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CompleteLegacyPurchaseCost {
	return &CompleteLegacyPurchaseCost{rows, purchases, uow, recorder}
}

type CompleteLegacyPurchaseCostInput struct {
	GymID, ActorUserID, MovementID uuid.UUID
	ActorRole                      string
	ExpectedVersion                int
	UnitCost                       float64
	Reason                         string
}

func (uc *CompleteLegacyPurchaseCost) Execute(ctx context.Context, in CompleteLegacyPurchaseCostInput) error {
	if in.ActorRole != "owner" {
		return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseOwnerRequired, "")
	}
	reason := strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 200 {
		return sharedDomain.NewValidationError(prodErrors.ErrPurchaseCorrectionReason)
	}
	if in.ExpectedVersion < 1 || !validAdjustmentCost(in.UnitCost) {
		return sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchaseCost)
	}
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		row, err := uc.Rows.GetLegacyPurchaseMovement(tx, in.GymID, in.MovementID)
		if errors.Is(err, prodErrors.ErrPurchaseNotFound) {
			return sharedDomain.NewBusinessError(err, "")
		}
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if row.Version != in.ExpectedVersion {
			return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseVersionConflict, "")
		}
		if row.Quantity <= 0 || (row.Cost != nil && *row.Cost > 0) {
			return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseAlreadyResolved, "")
		}
		p, err := uc.Purchases.GetByStockMovement(tx, in.GymID, in.MovementID)
		if err != nil && !errors.Is(err, prodErrors.ErrPurchaseNotFound) {
			return sharedDomain.NewUnexpectedError(err)
		}
		now := time.Now().UTC()
		if p != nil && p.UnitCost != nil && *p.UnitCost > 0 && p.TotalAmount != nil && *p.TotalAmount > 0 {
			if p.Status == purchaseDomain.StatusLegacyIncomplete && p.Quantity == row.Quantity && math.Round(*p.UnitCost*100) == math.Round(in.UnitCost*100) {
				return nil
			}
			return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseAlreadyResolved, "")
		}
		if p == nil {
			p, err = purchaseDomain.New(purchaseDomain.Input{ID: uuid.NewSHA1(row.MovementID, []byte("legacy-purchase-cost")), GymID: in.GymID, StockMovementID: row.MovementID, ProductID: row.ProductID, CreatedBy: in.ActorUserID, Quantity: row.Quantity, Status: purchaseDomain.StatusLegacyIncomplete, IdempotencyKey: "legacy-purchase-cost:" + row.MovementID.String(), Now: row.OccurredAt})
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if err = p.CompleteLegacyCost(in.UnitCost, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			p.Version = 1
			if _, err = uc.Purchases.Create(tx, p); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		} else {
			version := p.Version
			if p.Quantity != row.Quantity {
				return sharedDomain.NewBusinessError(prodErrors.ErrInvalidPurchase, "")
			}
			if err = p.CompleteLegacyCost(in.UnitCost, now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
			if _, err = uc.Purchases.Update(tx, p, version); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		return recordInventoryPurchaseAudit(ctx, uc.Audit, tx, p, in.ActorUserID, "complete_legacy_cost", map[string]any{"unit_cost": in.UnitCost, "total_amount": p.TotalAmount, "reason": reason, "stock_movement_id": row.MovementID}, now)
	})
}
