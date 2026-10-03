package app

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	cashclose "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CreateCashDrawerInput struct {
	GymID, ActorUserID uuid.UUID
	ActorRole          string
	Name               string
	IdempotencyKey     string
}

type UpdateCashDrawerInput struct {
	GymID, ActorUserID uuid.UUID
	ActorRole          string
	DrawerID           uuid.UUID
	Name               *string
	Active             *bool
	ExpectedVersion    int
}

type ListCashDrawersInput struct {
	GymID           uuid.UUID
	IncludeInactive bool
}

type CashDrawers struct {
	Repo  billingRepo.CashDrawerRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewCashDrawers(repo billingRepo.CashDrawerRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CashDrawers {
	return &CashDrawers{Repo: repo, UoW: uow, Audit: recorder}
}

// ValidateActiveCashDrawer lets physical-money writers in other bounded
// contexts depend on a tiny capability instead of the billing catalog type.
// It intentionally runs in the caller's transaction so validation and the
// resulting movement are atomic.
func (uc *CashDrawers) ValidateActiveCashDrawer(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID) error {
	drawer, err := uc.Repo.GetByID(tx, gymID, drawerID)
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	if drawer == nil {
		if drawerID == cashclose.DefaultDrawerID(gymID) {
			// Gyms created by pre-catalog builds use the deterministic main
			// drawer even before its catalog row is materialized.
			return nil
		}
		return sharedDomain.NewBusinessError(cashclose.ErrDrawerNotFound, "")
	}
	if !drawer.Active {
		return sharedDomain.NewBusinessError(cashclose.ErrDrawerInactive, "")
	}
	return nil
}

func (uc *CashDrawers) Create(ctx context.Context, in CreateCashDrawerInput) (*cashclose.CashDrawer, error) {
	if in.ActorRole != "owner" {
		return nil, sharedDomain.NewBusinessError(cashclose.ErrDrawerOwnerRequired, "")
	}
	drawer, err := cashclose.NewDrawer(in.GymID, in.Name, in.IdempotencyKey, time.Now().UTC())
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	var out *cashclose.CashDrawer
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		existing, getErr := uc.Repo.GetByIdempotencyKey(tx, in.GymID, strings.TrimSpace(in.IdempotencyKey))
		if getErr != nil {
			return sharedDomain.NewUnexpectedError(getErr)
		}
		if existing != nil {
			if existing.Name != drawer.Name {
				return sharedDomain.NewBusinessError(cashclose.ErrDrawerIdempotencyConflict, "")
			}
			out = existing
			return nil
		}
		created, createErr := uc.Repo.Create(tx, drawer)
		if createErr != nil {
			if errors.Is(createErr, cashclose.ErrDrawerNameConflict) {
				return sharedDomain.NewBusinessError(createErr, "")
			}
			return sharedDomain.NewUnexpectedError(createErr)
		}
		out = created
		return uc.record(ctx, tx, created, in.ActorUserID, audit.ActionCreate,
			map[string]any{"name": created.Name, "code": created.Code, "active": created.Active})
	})
	return out, err
}

func (uc *CashDrawers) Update(ctx context.Context, in UpdateCashDrawerInput) (*cashclose.CashDrawer, error) {
	if in.ActorRole != "owner" {
		return nil, sharedDomain.NewBusinessError(cashclose.ErrDrawerOwnerRequired, "")
	}
	if in.ExpectedVersion < 1 {
		return nil, sharedDomain.NewValidationError(cashclose.ErrDrawerVersionConflict)
	}
	if in.Name == nil && in.Active == nil {
		return nil, sharedDomain.NewValidationError(errors.New("indica name o active"))
	}
	var out *cashclose.CashDrawer
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		drawer, getErr := uc.Repo.GetByID(tx, in.GymID, in.DrawerID)
		if getErr != nil {
			return sharedDomain.NewUnexpectedError(getErr)
		}
		if drawer == nil {
			return sharedDomain.NewBusinessError(cashclose.ErrDrawerNotFound, "")
		}
		if drawer.Version != in.ExpectedVersion {
			return sharedDomain.NewBusinessError(cashclose.ErrDrawerVersionConflict, "")
		}
		now := time.Now().UTC()
		beforeName, beforeActive, beforeVersion := drawer.Name, drawer.Active, drawer.Version
		if in.Name != nil {
			if renameErr := drawer.Rename(*in.Name, now); renameErr != nil {
				return sharedDomain.NewValidationError(renameErr)
			}
		}
		if in.Active != nil && !*in.Active && drawer.Active {
			busy, busyErr := uc.Repo.HasOpenActivity(tx, in.GymID, in.DrawerID)
			if busyErr != nil {
				return sharedDomain.NewUnexpectedError(busyErr)
			}
			if busy {
				return sharedDomain.NewBusinessError(cashclose.ErrDrawerHasOpenActivity, "")
			}
		}
		if in.Active != nil {
			if activeErr := drawer.SetActive(*in.Active, now); activeErr != nil {
				return sharedDomain.NewBusinessError(activeErr, "")
			}
		}
		if drawer.Version == beforeVersion {
			out = drawer
			return nil
		}
		// One HTTP command is one aggregate revision even when it changes both
		// the display name and active flag.
		drawer.Version = beforeVersion + 1
		drawer.UpdatedAt = now
		updated, updateErr := uc.Repo.Update(tx, drawer, beforeVersion)
		if updateErr != nil {
			if errors.Is(updateErr, cashclose.ErrDrawerVersionConflict) || errors.Is(updateErr, cashclose.ErrDrawerNameConflict) {
				return sharedDomain.NewBusinessError(updateErr, "")
			}
			return sharedDomain.NewUnexpectedError(updateErr)
		}
		out = updated
		return uc.record(ctx, tx, updated, in.ActorUserID, audit.ActionUpdate, map[string]any{
			"name_before": beforeName, "name_after": updated.Name,
			"active_before": beforeActive, "active_after": updated.Active,
		})
	})
	return out, err
}

func (uc *CashDrawers) Deactivate(ctx context.Context, gymID, actorID uuid.UUID, actorRole string,
	drawerID uuid.UUID, expectedVersion int) (*cashclose.CashDrawer, error) {
	active := false
	return uc.Update(ctx, UpdateCashDrawerInput{GymID: gymID, ActorUserID: actorID,
		ActorRole: actorRole, DrawerID: drawerID, Active: &active, ExpectedVersion: expectedVersion})
}

func (uc *CashDrawers) List(ctx context.Context, in ListCashDrawersInput) ([]*cashclose.CashDrawer, error) {
	var out []*cashclose.CashDrawer
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		rows, listErr := uc.Repo.ListByGym(tx, in.GymID, in.IncludeInactive)
		if listErr != nil {
			return sharedDomain.NewUnexpectedError(listErr)
		}
		out = rows
		for _, row := range rows {
			if row.IsMain || row.ID == cashclose.DefaultDrawerID(in.GymID) {
				return nil
			}
		}
		// Defensive for a gym created by an old build before the seed trigger.
		out = append(out, cashclose.NewMainDrawer(in.GymID, time.Unix(0, 0)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsMain != out[j].IsMain {
			return out[i].IsMain
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (uc *CashDrawers) record(ctx context.Context, tx sharedDomain.Transaction, drawer *cashclose.CashDrawer,
	actorID uuid.UUID, action string, changes map[string]any) error {
	if uc.Audit == nil {
		return nil
	}
	if err := uc.Audit.Record(ctx, tx, audit.Entry{GymID: drawer.GymID, EntityType: "cash_drawers",
		EntityID: drawer.ID, Action: action, ActorUserID: &actorID, Changes: changes,
		IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: drawer.UpdatedAt}); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	return nil
}
