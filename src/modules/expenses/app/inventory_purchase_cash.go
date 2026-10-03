package app

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"

	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// InventoryPurchaseCashService is the narrow cross-context seam used by the
// products application service. It records one physical drawer outflow and
// classifies it immediately; it never creates a general Expense because the
// inventory-purchase aggregate owns the business meaning of this money.
type InventoryPurchaseCashService struct {
	Movements     expRepo.CashMovementRepository
	DrawerCatalog CashDrawerValidator
	Marker        CashSessionAdjustmentMarker
	Audit         audit.Recorder
}

func (s *InventoryPurchaseCashService) WithAudit(recorder audit.Recorder) *InventoryPurchaseCashService {
	s.Audit = recorder
	return s
}

func NewInventoryPurchaseCashService(movements expRepo.CashMovementRepository) *InventoryPurchaseCashService {
	return &InventoryPurchaseCashService{Movements: movements}
}

// UseExistingInventoryPurchaseCash links an already-recorded physical CashOut
// instead of creating a second outflow. Classification changes business
// meaning only; it deliberately does not invalidate a certified cash count
// because date, amount and drawer remain untouched.
func (s *InventoryPurchaseCashService) UseExistingInventoryPurchaseCash(
	ctx context.Context,
	tx sharedDomain.Transaction,
	movementID, gymID, operatorID uuid.UUID,
	requestedDrawerID *uuid.UUID,
	paidOn time.Time,
	amount float64,
	now time.Time,
) (uuid.UUID, error) {
	m, err := s.Movements.GetByID(tx, gymID, movementID)
	if err != nil {
		return uuid.Nil, sharedDomain.NewBusinessError(expErrors.ErrCashMovementNotFound, "")
	}
	drawerID := m.CashDrawerID
	if drawerID == uuid.Nil {
		drawerID = gymID
	}
	if err = validateRequestedCashDrawer(tx, s.DrawerCatalog, gymID, drawerID); err != nil {
		return uuid.Nil, err
	}
	compatible := m.ClassificationStatus == cashDomain.Unclassified && m.ExpenseID == nil &&
		m.MovementType == cashDomain.CashOut &&
		dateOnly(m.MovementOn).Equal(dateOnly(paidOn)) &&
		math.Round(m.Amount*100) == math.Round(amount*100)
	if requestedDrawerID != nil && *requestedDrawerID != uuid.Nil && drawerID != *requestedDrawerID {
		compatible = false
	}
	if !compatible {
		return uuid.Nil, sharedDomain.NewBusinessError(expErrors.ErrCashMovementNotCompatible, "")
	}
	version := m.Version
	if err = m.ClassifyInventoryPurchase(now); err != nil {
		return uuid.Nil, sharedDomain.NewBusinessError(expErrors.ErrCashMovementNotCompatible, "")
	}
	if _, err = s.Movements.Update(tx, m, version); err != nil {
		return uuid.Nil, sharedDomain.NewUnexpectedError(err)
	}
	if s.Audit != nil {
		if err = s.Audit.Record(ctx, tx, audit.Entry{
			GymID: gymID, EntityType: "cash_movements", EntityID: movementID,
			Action: "classify_as_inventory_purchase", ActorUserID: &operatorID,
			Changes: map[string]any{"classification_before": cashDomain.Unclassified,
				"classification_after": cashDomain.AsInventoryPurchase, "cash_drawer_id": drawerID},
			IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: now,
		}); err != nil {
			return uuid.Nil, sharedDomain.NewUnexpectedError(err)
		}
	}
	return drawerID, nil
}

func (s *InventoryPurchaseCashService) WithCashDrawerValidator(validator CashDrawerValidator) *InventoryPurchaseCashService {
	s.DrawerCatalog = validator
	return s
}

func (s *InventoryPurchaseCashService) WithCashSessionMarker(marker CashSessionAdjustmentMarker) *InventoryPurchaseCashService {
	s.Marker = marker
	return s
}

func (s *InventoryPurchaseCashService) RecordInventoryPurchase(
	_ context.Context,
	tx sharedDomain.Transaction,
	movementID, gymID, operatorID, drawerID uuid.UUID,
	paidOn time.Time,
	amount float64,
	reason string,
	now time.Time,
) (uuid.UUID, error) {
	if drawerID == uuid.Nil {
		drawerID = gymID
	}
	if err := validateRequestedCashDrawer(tx, s.DrawerCatalog, gymID, drawerID); err != nil {
		return uuid.Nil, err
	}
	m, err := cashDomain.New(movementID, gymID, operatorID, paidOn, amount,
		cashDomain.CashOut, reason, now)
	if err != nil {
		return uuid.Nil, err
	}
	if err := m.ClassifyInventoryPurchase(now); err != nil {
		return uuid.Nil, err
	}
	m.WithCashDrawer(drawerID)
	if _, err := s.Movements.Create(tx, m); err != nil {
		return uuid.Nil, err
	}
	return m.ID, nil
}

func (s *InventoryPurchaseCashService) InventoryPurchaseUsesCashDrawer(
	tx sharedDomain.Transaction, gymID, movementID, drawerID uuid.UUID,
) (bool, error) {
	if drawerID == uuid.Nil {
		drawerID = gymID
	}
	m, err := s.Movements.GetByID(tx, gymID, movementID)
	if err != nil {
		return false, err
	}
	actual := m.CashDrawerID
	if actual == uuid.Nil {
		actual = gymID
	}
	return m.ClassificationStatus == cashDomain.AsInventoryPurchase && actual == drawerID, nil
}

func (s *InventoryPurchaseCashService) CashDrawerForInventoryPurchase(
	tx sharedDomain.Transaction, gymID, movementID uuid.UUID,
) (*uuid.UUID, error) {
	m, err := s.Movements.GetByID(tx, gymID, movementID)
	if err != nil {
		return nil, err
	}
	drawerID := m.CashDrawerID
	if drawerID == uuid.Nil {
		drawerID = gymID
	}
	return &drawerID, nil
}

func (s *InventoryPurchaseCashService) VoidInventoryPurchaseCash(
	ctx context.Context, tx sharedDomain.Transaction, gymID, actorID, movementID uuid.UUID,
	correctionReason string, now time.Time,
) error {
	m, err := s.Movements.GetByID(tx, gymID, movementID)
	if err != nil {
		return err
	}
	if m.ClassificationStatus != cashDomain.AsInventoryPurchase || m.ExpenseID != nil {
		return expErrors.ErrLinkedCashMovement
	}
	version := m.Version
	movementOn, recordedAt := m.MovementOn, m.CreatedAt
	drawerID := m.CashDrawerID
	if drawerID == uuid.Nil {
		drawerID = gymID
	}
	m.SoftDelete(now)
	if _, err = s.Movements.Update(tx, m, version); err != nil {
		return err
	}
	if s.Marker != nil {
		return s.Marker.MarkPhysicalCashAdjustment(ctx, tx, CashSessionAdjustmentInput{
			GymID: gymID, ActorUserID: actorID, OperationalDate: movementOn,
			OriginalRecordedAt: recordedAt, DrawerID: drawerID, Reason: correctionReason,
		})
	}
	return nil
}
