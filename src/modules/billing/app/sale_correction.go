package app

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	saleDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/sale"
	correctionDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/salecorrection"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CorrectSaleLineInput struct {
	SaleItemID *uuid.UUID
	ProductID  uuid.UUID
	Quantity   int
}

type CorrectSaleInput struct {
	GymID, ActorUserID, SaleID uuid.UUID
	ActorRole                  string
	ExpectedVersion            int
	Annul                      bool
	Reason, MoneyResolution    string
	IncreaseResolution         string
	RefundMethod               string
	RefundCashDrawerID         *uuid.UUID
	CollectionMethod           string
	CollectionCashDrawerID     *uuid.UUID
	CollectionDate             time.Time
	IdempotencyKey             string
	Lines                      []CorrectSaleLineInput
}

type SaleMoneyEffect struct {
	PreviousTotal                 float64    `json:"previous_total"`
	CorrectedTotal                float64    `json:"corrected_total"`
	PhysicalCollected             float64    `json:"physical_collected"`
	RecognizedIncome              float64    `json:"recognized_income"`
	RefundedNow                   float64    `json:"refunded_now"`
	PendingRefundDue              float64    `json:"pending_refund_due"`
	RefundID                      *uuid.UUID `json:"refund_id,omitempty"`
	RefundMethod                  string     `json:"refund_method,omitempty"`
	AdditionalCollectedNow        float64    `json:"additional_collected_now"`
	AdditionalPending             float64    `json:"additional_pending"`
	AdditionalHistoricalCollected float64    `json:"additional_historical_collected"`
	SettlementID                  *uuid.UUID `json:"settlement_id,omitempty"`
	Status                        string     `json:"status"`
}

type CorrectSaleOutput struct {
	CorrectionID     uuid.UUID             `json:"correction_id"`
	SaleVersion      int                   `json:"sale_version"`
	CorrectionType   string                `json:"correction_type"`
	Annulled         bool                  `json:"annulled"`
	InventoryEffects []SaleInventoryEffect `json:"inventory_effects"`
	MoneyEffect      SaleMoneyEffect       `json:"money_effect"`
}

type SaleInventoryEffect struct {
	ProductID  uuid.UUID `json:"product_id"`
	StockDelta int       `json:"stock_delta"`
}

type CorrectSale struct {
	Sales       billingRepo.SaleRepository
	SaleItems   billingRepo.SaleItemRepository
	Payments    billingRepo.PaymentRepository
	Corrections billingRepo.SaleCorrectionStore
	Refunds     billingRepo.RefundRepository
	Products    *prodApp.ProductService
	Folios      *folioSvc.Generator
	CashMarker  CashSessionCorrectionMarker
	Gyms        gymRepo.GymRepository
	CashDrawers CashDrawerValidator
	UoW         sharedDomain.UnitOfWork
	Audit       audit.Recorder
}

func (uc *CorrectSale) WithGyms(gyms gymRepo.GymRepository) *CorrectSale {
	uc.Gyms = gyms
	return uc
}

func (uc *CorrectSale) WithCashDrawers(v CashDrawerValidator) *CorrectSale {
	uc.CashDrawers = v
	return uc
}

func NewCorrectSale(sales billingRepo.SaleRepository, items billingRepo.SaleItemRepository,
	payments billingRepo.PaymentRepository, corrections billingRepo.SaleCorrectionStore,
	refunds billingRepo.RefundRepository, products *prodApp.ProductService, folios *folioSvc.Generator,
	cashMarker CashSessionCorrectionMarker, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CorrectSale {
	return &CorrectSale{Sales: sales, SaleItems: items, Payments: payments, Corrections: corrections,
		Refunds: refunds, Products: products, Folios: folios, CashMarker: cashMarker, UoW: uow, Audit: recorder}
}

func (uc *CorrectSale) Execute(ctx context.Context, in CorrectSaleInput) (*CorrectSaleOutput, error) {
	if strings.TrimSpace(in.Reason) == "" {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrSaleCorrectionReasonRequired)
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrIdempotencyKeyRequired)
	}
	if !in.Annul && len(in.Lines) == 0 {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrSaleEmpty)
	}
	if in.Annul && len(in.Lines) != 0 {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrSaleCorrectionResolutionInvalid)
	}
	if in.ActorRole != "owner" && in.MoneyResolution != correctionDomain.RecordOnly {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrSaleCorrectionForbidden, "")
	}
	if in.ActorRole != "owner" && in.IncreaseResolution != "" && in.IncreaseResolution != correctionDomain.IncreasePending {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrSaleCorrectionForbidden, "")
	}
	if in.MoneyResolution == correctionDomain.RefundExcess && !validRealRefundMethod(in.RefundMethod) {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodInvalid)
	}
	if in.IncreaseResolution == correctionDomain.IncreaseCollectNow && !validRealRefundMethod(in.CollectionMethod) {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodInvalid)
	}
	fingerprint, err := saleCorrectionFingerprint(in)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	var out CorrectSaleOutput
	now := time.Now().UTC()
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		if in.IncreaseResolution == correctionDomain.IncreaseCollectNow {
			collectionDay, dateErr := resolveMonetaryDate(tx, uc.Gyms, in.GymID, in.CollectionDate, now)
			if dateErr != nil {
				return dateErr
			}
			in.CollectionDate = collectionDay
		}
		existing, err := uc.Corrections.GetCorrectionByIdempotencyKey(tx, in.GymID, strings.TrimSpace(in.IdempotencyKey))
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if existing != nil {
			if existing.IdempotencyFingerprint != fingerprint {
				return sharedDomain.NewBusinessError(billingErrors.ErrIdempotencyKeyConflict, "")
			}
			if len(existing.IdempotencyResult) == 0 || json.Unmarshal(existing.IdempotencyResult, &out) != nil {
				return sharedDomain.NewUnexpectedError(errors.New("sale correction idempotency result is missing or invalid"))
			}
			return nil
		}
		if err := validateRequestedCashDrawer(uc.CashDrawers, tx, in.GymID, in.RefundMethod, in.RefundCashDrawerID); err != nil {
			return err
		}
		if err := validateRequestedCashDrawer(uc.CashDrawers, tx, in.GymID, in.CollectionMethod, in.CollectionCashDrawerID); err != nil {
			return err
		}

		sale, err := uc.Sales.GetByID(tx, in.SaleID)
		if err != nil {
			return err
		}
		if sale.GymID != in.GymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}
		if sale.CorrectionVersion != in.ExpectedVersion {
			return sharedDomain.NewBusinessError(billingErrors.ErrSaleVersionConflict, "")
		}
		payment, err := uc.Payments.GetByID(tx, sale.PaymentID)
		if err != nil {
			return err
		}
		if payment.GymID != in.GymID || payment.Concept != paymentDomain.ConceptProduct {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}
		if in.ActorRole != "owner" && payment.OperatorID != in.ActorUserID {
			return sharedDomain.NewBusinessError(billingErrors.ErrSaleCorrectionForbidden, "")
		}
		balance := billingRepo.RefundBalance{Root: payment, Collected: payment.Amount,
			Recognized: payment.RecognizedAmount, Refundable: payment.Amount}
		if locker, ok := uc.Payments.(billingRepo.PaymentRefundLocker); ok {
			balance, err = locker.RefundBalanceForUpdate(tx, in.GymID, payment.ID)
			if err != nil {
				return err
			}
			if balance.Refunded != 0 {
				return sharedDomain.NewBusinessError(billingErrors.ErrSaleCorrectionUnsupported, "")
			}
		}

		oldItems, err := uc.SaleItems.ListBySale(tx, sale.ID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		before, err := correctionSnapshot(sale, oldItems, payment)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		persistItems, desired, err := uc.buildCorrectedItems(tx, in, sale, oldItems, now)
		if err != nil {
			return err
		}
		var subtotal float64
		for _, item := range desired {
			subtotal += item.LineTotal
		}
		subtotal = roundMoney(subtotal)
		// The correction changes the cart, not the commercial treatment that
		// was agreed for it. Preserve the original effective discount rate
		// instead of carrying an absolute discount into a much smaller cart.
		newDiscount := proportionalDiscount(sale.Subtotal, sale.Discount, subtotal)
		oldTotal := sale.Total
		newTotal := roundMoney(subtotal - newDiscount)
		obligationDelta := roundMoney(newTotal - oldTotal)
		increaseAmount := roundMoney(math.Max(obligationDelta, 0))
		excess := roundMoney(math.Max(balance.Collected-newTotal, 0))
		if increaseAmount > 0 {
			if in.Annul || in.MoneyResolution != correctionDomain.RecordOnly ||
				(in.IncreaseResolution != correctionDomain.IncreasePending &&
					in.IncreaseResolution != correctionDomain.IncreaseAlreadyCollected &&
					in.IncreaseResolution != correctionDomain.IncreaseCollectNow) {
				return sharedDomain.NewValidationError(billingErrors.ErrSaleCorrectionResolutionInvalid)
			}
			if in.ActorRole != "owner" && in.IncreaseResolution != correctionDomain.IncreasePending {
				return sharedDomain.NewBusinessError(billingErrors.ErrSaleCorrectionForbidden, "")
			}
			if in.IncreaseResolution == correctionDomain.IncreasePending && sale.MemberID == nil {
				return sharedDomain.NewBusinessError(billingErrors.ErrCreditRequiresMember, "")
			}
		} else {
			if in.IncreaseResolution != "" {
				return sharedDomain.NewValidationError(billingErrors.ErrSaleCorrectionResolutionInvalid)
			}
			if in.MoneyResolution != correctionDomain.RecordOnly && excess <= 0 {
				return sharedDomain.NewValidationError(billingErrors.ErrSaleCorrectionResolutionInvalid)
			}
		}

		// Payments are cash-basis. Existing settlements remain immutable, so a
		// correction rewrites only the root row and derives the new debt from the
		// corrected obligation. record_only means the recorded physical amount was
		// wrong too; refund_* keeps physical collection intact and recognizes only
		// what the corrected sale actually earned.
		childPhysical := roundMoney(balance.Collected - payment.Amount)
		childRecognized := roundMoney(balance.Recognized - payment.RecognizedAmount)
		if in.Annul && (childPhysical != 0 || childRecognized != 0) {
			return sharedDomain.NewBusinessError(billingErrors.ErrSaleCorrectionUnsupported, "")
		}
		correctedCollected := balance.Collected
		rootPhysical := payment.Amount
		additionalNow, additionalPending, additionalHistorical := 0.0, 0.0, 0.0
		annulRecordOnly := in.Annul && in.MoneyResolution == correctionDomain.RecordOnly
		switch {
		case annulRecordOnly:
			correctedCollected = 0
		case increaseAmount > 0 && in.IncreaseResolution == correctionDomain.IncreasePending:
			additionalPending = increaseAmount
		case increaseAmount > 0 && in.IncreaseResolution == correctionDomain.IncreaseAlreadyCollected:
			correctedCollected = roundMoney(correctedCollected + increaseAmount)
			rootPhysical = roundMoney(rootPhysical + increaseAmount)
			additionalHistorical = increaseAmount
		case increaseAmount > 0 && in.IncreaseResolution == correctionDomain.IncreaseCollectNow:
			additionalNow = increaseAmount
		case in.MoneyResolution == correctionDomain.RecordOnly && excess > 0:
			correctedCollected = newTotal
			rootPhysical = roundMoney(newTotal - childPhysical)
		}
		recognizedTotal := roundMoney(math.Min(correctedCollected, newTotal))
		rootRecognized := roundMoney(recognizedTotal - childRecognized)
		newBalancePending := roundMoney(math.Max(newTotal-correctedCollected, 0))
		if !annulRecordOnly && (rootPhysical < 0 || rootRecognized < 0 || rootRecognized > rootPhysical) {
			// The corrected total fell below already-recognized child settlements.
			// Rewriting those append-only rows needs an allocation ledger; fail closed
			// instead of manufacturing a negative root payment.
			return sharedDomain.NewBusinessError(billingErrors.ErrSaleCorrectionUnsupported, "")
		}

		inventoryEffects, err := uc.adjustCorrectionStock(ctx, tx, in, oldItems, desired, now)
		if err != nil {
			return err
		}
		sale.Subtotal, sale.Discount, sale.Total, sale.Items = subtotal, newDiscount, newTotal, desired
		sale.Version++
		sale.CorrectionVersion++
		sale.UpdatedAt = now
		if in.Annul {
			sale.DeletedAt = &now
		}
		breakdown := make([]paymentDomain.BreakdownLine, 0, len(desired))
		for _, item := range desired {
			breakdown = append(breakdown, paymentDomain.BreakdownLine{Label: saleLineLabel(item), Amount: item.LineTotal})
		}
		if annulRecordOnly {
			payment.BalancePending = 0
			payment.Version++
			payment.UpdatedAt = now
			payment.DeletedAt = &now
		} else if err := payment.CorrectSaleAmounts(rootPhysical, rootRecognized, newBalancePending, newDiscount, breakdown, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		writer, ok := uc.Payments.(billingRepo.PaymentCorrectionStore)
		if !ok {
			return sharedDomain.NewUnexpectedError(errors.New("payment correction store is not configured"))
		}
		if err := uc.Corrections.ApplySaleCorrection(tx, sale, persistItems, in.ExpectedVersion); err != nil {
			return err
		}
		if _, err := writer.UpdateForSaleCorrection(tx, payment); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var collectionSettlementID *uuid.UUID
		if increaseAmount > 0 && in.IncreaseResolution == correctionDomain.IncreaseCollectNow {
			settlement, err := uc.createCorrectionCollection(ctx, tx, payment, increaseAmount, in, now)
			if err != nil {
				return err
			}
			collectionSettlementID = &settlement.ID
			correctedCollected = roundMoney(correctedCollected + increaseAmount)
			recognizedTotal = roundMoney(recognizedTotal + increaseAmount)
		}
		after, err := correctionSnapshot(sale, desired, payment)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		correction, err := correctionDomain.New(correctionDomain.Input{
			ID: uuid.New(), GymID: in.GymID, SaleID: sale.ID, CreatedBy: in.ActorUserID,
			ExpectedSaleVersion: in.ExpectedVersion, Reason: in.Reason,
			CorrectionType: correctionType(in.Annul), MoneyResolution: in.MoneyResolution,
			IncreaseResolution: in.IncreaseResolution,
			MonetaryDelta:      correctionMonetaryDelta(increaseAmount, excess), BeforeSnapshot: before, AfterSnapshot: after,
			IdempotencyKey: in.IdempotencyKey, IdempotencyFingerprint: fingerprint, Now: now,
		})
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Corrections.CreateCorrection(tx, correction); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		if (in.MoneyResolution == correctionDomain.RecordOnly || in.IncreaseResolution == correctionDomain.IncreaseAlreadyCollected) &&
			payment.PaymentMethod == paymentDomain.MethodCash && uc.CashMarker != nil {
			if err := uc.CashMarker.MarkRecordCorrection(ctx, tx, CashSessionAdjustmentInput{
				GymID: in.GymID, ActorUserID: in.ActorUserID, OperationalDate: payment.PaymentDate,
				OriginalRecordedAt: payment.CreatedAt, Reason: correction.Reason, DrawerID: payment.CashDrawerID,
			}); err != nil {
				return err
			}
		}

		refundedNow, pending := 0.0, 0.0
		if in.MoneyResolution == correctionDomain.RefundExcess {
			refundDay, dateErr := resolveMonetaryDate(tx, uc.Gyms, in.GymID, time.Time{}, now)
			if dateErr != nil {
				return dateErr
			}
			settlement, err := uc.createOvercollectionSettlement(ctx, tx, correction, payment, excess, in.RefundMethod, in.RefundCashDrawerID,
				refundDay, in.IdempotencyKey+":settlement", now)
			if err != nil {
				return err
			}
			refundedNow = excess
			out.MoneyEffect.RefundID = &settlement.ID
		} else if in.MoneyResolution == correctionDomain.RefundPending {
			pending = excess
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{GymID: in.GymID, EntityType: "sale_corrections", EntityID: correction.ID,
			Action: audit.ActionCreate, ActorUserID: &in.ActorUserID, Changes: map[string]any{
				"sale_id": sale.ID, "old_total": oldTotal, "new_total": newTotal, "obligation_delta": obligationDelta,
				"correction_type": correction.CorrectionType, "money_resolution": in.MoneyResolution,
				"increase_resolution": in.IncreaseResolution, "pending_refund_due": pending,
			}, IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: now})
		refundID := out.MoneyEffect.RefundID
		out = CorrectSaleOutput{CorrectionID: correction.ID, SaleVersion: sale.CorrectionVersion,
			CorrectionType: correction.CorrectionType, Annulled: in.Annul, InventoryEffects: inventoryEffects,
			MoneyEffect: SaleMoneyEffect{PreviousTotal: oldTotal, CorrectedTotal: newTotal,
				PhysicalCollected: correctedCollected, RecognizedIncome: recognizedTotal, RefundedNow: refundedNow,
				PendingRefundDue: pending, RefundID: refundID, RefundMethod: in.RefundMethod,
				AdditionalCollectedNow: additionalNow, AdditionalPending: additionalPending,
				AdditionalHistoricalCollected: additionalHistorical, SettlementID: collectionSettlementID,
				Status: correctionMoneyStatus(in, pending)}}
		result, err := json.Marshal(out)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		correction.IdempotencyResult = result
		if err := uc.Corrections.FinalizeCorrectionIdempotency(tx, correction); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		return nil
	})
	if err != nil {
		// Two devices can pass the initial idempotency lookup before either
		// commits. The losing transaction is rolled back by Command; only then
		// do a fresh read and turn a same-command race into the exact replay.
		// A different command with the same key remains a conflict, while a
		// different key keeps the optimistic sale-version error.
		if replay, replayErr := uc.replayCommittedCorrection(ctx, in.GymID, in.IdempotencyKey, fingerprint); replayErr != nil {
			return nil, replayErr
		} else if replay != nil {
			return replay, nil
		}
		return nil, err
	}
	return &out, nil
}

func (uc *CorrectSale) replayCommittedCorrection(ctx context.Context, gymID uuid.UUID, key, fingerprint string) (*CorrectSaleOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, nil // preserve the original command error
	}
	existing, err := uc.Corrections.GetCorrectionByIdempotencyKey(tx, gymID, strings.TrimSpace(key))
	if err != nil || existing == nil {
		return nil, nil
	}
	if existing.IdempotencyFingerprint != fingerprint {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrIdempotencyKeyConflict, "")
	}
	var out CorrectSaleOutput
	if len(existing.IdempotencyResult) == 0 || json.Unmarshal(existing.IdempotencyResult, &out) != nil {
		return nil, sharedDomain.NewUnexpectedError(errors.New("sale correction idempotency result is missing or invalid"))
	}
	return &out, nil
}

func (uc *CorrectSale) buildCorrectedItems(tx sharedDomain.Transaction, in CorrectSaleInput, sale *saleDomain.Sale, old []*saleDomain.SaleItem, now time.Time) ([]*saleDomain.SaleItem, []*saleDomain.SaleItem, error) {
	byID := make(map[uuid.UUID]*saleDomain.SaleItem, len(old))
	for _, item := range old {
		byID[item.ID] = item
	}
	seenIDs, seenProducts := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	desired := make([]*saleDomain.SaleItem, 0, len(in.Lines))
	for _, line := range in.Lines {
		if line.ProductID == uuid.Nil || line.Quantity <= 0 || seenProducts[line.ProductID] {
			return nil, nil, sharedDomain.NewValidationError(billingErrors.ErrSaleItemQuantityInvalid)
		}
		seenProducts[line.ProductID] = true
		var item *saleDomain.SaleItem
		if line.SaleItemID != nil {
			base := byID[*line.SaleItemID]
			if base == nil || seenIDs[base.ID] {
				return nil, nil, sharedDomain.NewValidationError(billingErrors.ErrSaleItemQuantityInvalid)
			}
			seenIDs[base.ID] = true
			item = cloneSaleItem(base)
			if base.ProductID != line.ProductID {
				snapshot, err := uc.Products.SnapshotForSaleCorrection(tx, in.GymID, line.ProductID)
				if err != nil {
					return nil, nil, err
				}
				item.ProductID, item.ProductNameSnapshot = line.ProductID, snapshot.Product.Name
				item.UnitPriceSnapshot, item.UnitCostSnapshot = snapshot.Product.Price, snapshot.UnitCost
			}
			item.Quantity, item.LineTotal = line.Quantity, roundMoney(item.UnitPriceSnapshot*float64(line.Quantity))
			item.Version++
			item.UpdatedAt = now
		} else {
			snapshot, err := uc.Products.SnapshotForSaleCorrection(tx, in.GymID, line.ProductID)
			if err != nil {
				return nil, nil, err
			}
			item, err = saleDomain.NewItem(uuid.New(), in.GymID, sale.ID, line.ProductID,
				snapshot.Product.Name, snapshot.Product.Price, snapshot.UnitCost, line.Quantity, now)
			if err != nil {
				return nil, nil, sharedDomain.NewValidationError(err)
			}
		}
		desired = append(desired, item)
	}
	persist := append([]*saleDomain.SaleItem(nil), desired...)
	for _, oldItem := range old {
		if seenIDs[oldItem.ID] {
			continue
		}
		tombstone := cloneSaleItem(oldItem)
		tombstone.Version++
		tombstone.UpdatedAt = now
		tombstone.DeletedAt = &now
		persist = append(persist, tombstone)
	}
	return persist, desired, nil
}

func (uc *CorrectSale) adjustCorrectionStock(ctx context.Context, tx sharedDomain.Transaction, in CorrectSaleInput, old, desired []*saleDomain.SaleItem, now time.Time) ([]SaleInventoryEffect, error) {
	counts := map[uuid.UUID]int{}
	lineIDs := map[uuid.UUID]uuid.UUID{}
	for _, item := range old {
		counts[item.ProductID] += item.Quantity
		lineIDs[item.ProductID] = item.ID
	}
	for _, item := range desired {
		counts[item.ProductID] -= item.Quantity
		lineIDs[item.ProductID] = item.ID
	}
	productIDs := make([]uuid.UUID, 0, len(counts))
	for productID := range counts {
		productIDs = append(productIDs, productID)
	}
	sort.Slice(productIDs, func(i, j int) bool { return productIDs[i].String() < productIDs[j].String() })
	effects := make([]SaleInventoryEffect, 0, len(productIDs))
	for _, productID := range productIDs {
		delta := counts[productID]
		if delta == 0 {
			continue
		}
		if err := uc.Products.AdjustForSaleCorrection(ctx, tx, prodApp.AdjustForSaleCorrectionInput{
			GymID: in.GymID, ProductID: productID, OperatorID: in.ActorUserID,
			SaleItemID: lineIDs[productID], StockDelta: delta, Reason: "Corrección de venta: " + strings.TrimSpace(in.Reason),
		}, now); err != nil {
			return nil, err
		}
		effects = append(effects, SaleInventoryEffect{ProductID: productID, StockDelta: delta})
	}
	return effects, nil
}

func (uc *CorrectSale) createOvercollectionSettlement(ctx context.Context, tx sharedDomain.Transaction,
	correction *correctionDomain.Correction, parent *paymentDomain.Payment, amount float64, method string,
	cashDrawerID *uuid.UUID, day time.Time, key string, now time.Time) (*paymentDomain.Payment, error) {
	if !validRealRefundMethod(method) {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodInvalid)
	}
	resolvedDay, err := resolveMonetaryDate(tx, uc.Gyms, parent.GymID, day, now)
	if err != nil {
		return nil, err
	}
	if err := validateMonetaryChronology(resolvedDay, parent.PaymentDate); err != nil {
		return nil, err
	}
	day = resolvedDay
	folio, err := uc.Folios.Next(tx, parent.GymID, paymentDomain.ConceptRefund)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	refundPayment, err := paymentDomain.NewRefundPaymentWithin(uuid.New(), parent.GymID, correction.CreatedBy,
		parent, folio, amount, amount, method, correction.Reason, day, now)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	if method == paymentDomain.MethodCash {
		drawerID := parent.GymID
		if cashDrawerID != nil {
			drawerID = *cashDrawerID
		}
		refundPayment.WithCashDrawer(drawerID)
	}
	parent.Touch(now)
	if _, err := uc.Payments.Update(tx, parent); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if _, err := uc.Payments.Create(tx, refundPayment); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	refundPaymentID := refundPayment.ID
	aggregateID := uuid.New()
	fingerprint, err := refundCommandFingerprint(RefundPaymentInput{
		GymID: parent.GymID, ActorUserID: correction.CreatedBy, ParentPaymentID: parent.ID,
		Reason: correction.Reason, Method: method, CashDrawerID: cashDrawerID, Amount: amount,
		PaymentDate: day, SaleID: &correction.SaleID, CorrectionID: &correction.ID, Kind: refundDomain.KindOvercollectionSettlement,
		MonetaryAmount: amount,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	result, err := json.Marshal(RefundPaymentOutput{
		RefundID: aggregateID, RefundPaymentID: &refundPaymentID,
		RefundFolio: refundPayment.Folio, Amount: amount,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	aggregate, err := refundDomain.New(refundDomain.Input{ID: aggregateID, GymID: parent.GymID,
		RootPaymentID: parent.ID, RefundPaymentID: &refundPaymentID, CreatedBy: correction.CreatedBy,
		SaleID: &correction.SaleID, Amount: amount, Method: method, RefundedOn: day, Reason: correction.Reason,
		Kind: refundDomain.KindOvercollectionSettlement, CorrectionID: &correction.ID,
		IdempotencyKey: key, IdempotencyFingerprint: fingerprint, IdempotencyResult: result, Now: now})
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	if _, err := uc.Refunds.Create(tx, aggregate); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return refundPayment, nil
}

func (uc *CorrectSale) createCorrectionCollection(ctx context.Context, tx sharedDomain.Transaction,
	parent *paymentDomain.Payment, amount float64, in CorrectSaleInput, now time.Time) (*paymentDomain.Payment, error) {
	day, err := resolveMonetaryDate(tx, uc.Gyms, in.GymID, in.CollectionDate, now)
	if err != nil {
		return nil, err
	}
	if err := validateMonetaryChronology(day, parent.PaymentDate); err != nil {
		return nil, err
	}
	folio, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptBalanceSettlement)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	note := strings.TrimSpace(in.Reason)
	settlement, err := paymentDomain.NewBalanceSettlementPayment(uuid.New(), in.GymID, in.ActorUserID,
		parent, folio, amount, in.CollectionMethod, day, now, &note)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	if in.CollectionMethod == paymentDomain.MethodCash {
		drawerID := in.GymID
		if in.CollectionCashDrawerID != nil {
			drawerID = *in.CollectionCashDrawerID
		}
		settlement.WithCashDrawer(drawerID)
	}
	if _, err := parent.DecrementBalance(amount, now); err != nil {
		return nil, sharedDomain.NewBusinessError(err, "")
	}
	if _, err := uc.Payments.Update(tx, parent); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if _, err := uc.Payments.Create(tx, settlement); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	_ = uc.Audit.Record(ctx, tx, audit.Entry{GymID: in.GymID, EntityType: "payments", EntityID: settlement.ID,
		Action: audit.ActionCreate, ActorUserID: &in.ActorUserID, Changes: map[string]any{
			"concept": paymentDomain.ConceptBalanceSettlement, "parent_payment_id": parent.ID,
			"amount": amount, "source": "sale_correction",
		}, IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: now})
	return settlement, nil
}

type SettlePendingRefundInput struct {
	GymID, ActorUserID, CorrectionID  uuid.UUID
	ActorRole, Method, IdempotencyKey string
	CashDrawerID                      *uuid.UUID
	PaymentDate                       time.Time
}
type SettlePendingRefundOutput struct {
	RefundID      uuid.UUID
	Amount        float64
	PaymentMethod string
	RefundedOn    time.Time
	PendingAmount float64
}

type SaleDetailPayment struct {
	Amount, RecognizedAmount float64
	PaymentMethod            string
	PaymentDate              time.Time
}
type SaleDetailLine struct {
	SaleItemID, ProductID                          uuid.UUID
	ProductName                                    string
	UnitPrice                                      float64
	Quantity, RefundedQuantity, RefundableQuantity int
	LineTotal                                      float64
}
type PendingRefundDetail struct {
	CorrectionID uuid.UUID
	AmountDue    float64
	CreatedAt    time.Time
	Reason       string
}
type SaleDetailOutput struct {
	ID, PaymentID                                   uuid.UUID
	MemberID                                        *uuid.UUID
	Version                                         int
	Folio                                           string
	Collected, Refunded, Refundable, BalancePending float64
	Payment                                         SaleDetailPayment
	Subtotal, Discount, Total                       float64
	Lines                                           []SaleDetailLine
	PendingRefundDue                                float64
	PendingRefunds                                  []PendingRefundDetail
}

func (uc *CorrectSale) Detail(ctx context.Context, gymID, saleID uuid.UUID) (*SaleDetailOutput, error) {
	var out SaleDetailOutput
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		sale, err := uc.Sales.GetByID(tx, saleID)
		if err != nil {
			return err
		}
		if sale.GymID != gymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}
		payment, err := uc.Payments.GetByID(tx, sale.PaymentID)
		if err != nil {
			return err
		}
		items, err := uc.SaleItems.ListBySale(tx, sale.ID)
		if err != nil {
			return err
		}
		used, err := uc.Refunds.RefundedQuantitiesBySale(tx, gymID, sale.ID)
		if err != nil {
			return err
		}
		balance := billingRepo.RefundBalance{Collected: payment.Amount, Refunded: 0, Refundable: payment.Amount}
		if reader, ok := uc.Payments.(billingRepo.PaymentRefundReader); ok {
			balance, err = reader.RefundBalance(tx, gymID, payment.ID)
			if err != nil {
				return err
			}
		}
		out = SaleDetailOutput{ID: sale.ID, PaymentID: sale.PaymentID, MemberID: sale.MemberID, Version: sale.CorrectionVersion,
			Folio: payment.Folio, Collected: balance.Collected, Refunded: balance.Refunded,
			Refundable: balance.Refundable, BalancePending: payment.BalancePending,
			Payment: SaleDetailPayment{Amount: payment.Amount, RecognizedAmount: payment.RecognizedAmount,
				PaymentMethod: payment.PaymentMethod, PaymentDate: payment.PaymentDate},
			Subtotal: sale.Subtotal, Discount: sale.Discount, Total: sale.Total}
		for _, item := range items {
			refunded := used[item.ID]
			out.Lines = append(out.Lines, SaleDetailLine{SaleItemID: item.ID, ProductID: item.ProductID,
				ProductName: item.ProductNameSnapshot, UnitPrice: item.UnitPriceSnapshot,
				Quantity: item.Quantity, LineTotal: item.LineTotal, RefundedQuantity: refunded,
				RefundableQuantity: item.Quantity - refunded})
		}
		corrections, err := uc.Corrections.ListCorrectionsBySale(tx, gymID, sale.ID)
		if err != nil {
			return err
		}
		for _, correction := range corrections {
			due, err := uc.Corrections.PendingAmount(tx, gymID, correction.ID, false)
			if err != nil {
				if errors.Is(err, billingErrors.ErrPendingRefundNotFound) {
					continue
				}
				return err
			}
			if due <= 0 {
				continue
			}
			out.PendingRefundDue += due
			out.PendingRefunds = append(out.PendingRefunds, PendingRefundDetail{CorrectionID: correction.ID,
				AmountDue: due, CreatedAt: correction.CreatedAt, Reason: correction.Reason})
		}
		out.PendingRefundDue = roundMoney(out.PendingRefundDue)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (uc *CorrectSale) SettlePending(ctx context.Context, in SettlePendingRefundInput) (*SettlePendingRefundOutput, error) {
	if in.ActorRole != "owner" {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrSaleCorrectionForbidden, "")
	}
	if !validRealRefundMethod(in.Method) {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodInvalid)
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrIdempotencyKeyRequired)
	}
	var out SettlePendingRefundOutput
	now := time.Now().UTC()
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		paymentDay, dateErr := resolveMonetaryDate(tx, uc.Gyms, in.GymID, in.PaymentDate, now)
		if dateErr != nil {
			return dateErr
		}
		in.PaymentDate = paymentDay
		if existing, err := uc.Refunds.GetByIdempotencyKey(tx, in.GymID, in.IdempotencyKey); err != nil {
			return err
		} else if existing != nil {
			if existing.CorrectionID == nil || *existing.CorrectionID != in.CorrectionID || existing.Kind != refundDomain.KindOvercollectionSettlement {
				return sharedDomain.NewBusinessError(billingErrors.ErrRefundIdempotencyConflict, "")
			}
			due, _ := uc.Corrections.PendingAmount(tx, in.GymID, in.CorrectionID, false)
			if existing.RefundPaymentID == nil {
				return sharedDomain.NewUnexpectedError(errors.New("overcollection settlement has no refund payment"))
			}
			out = SettlePendingRefundOutput{RefundID: *existing.RefundPaymentID, Amount: existing.Amount, PaymentMethod: existing.Method, RefundedOn: existing.RefundedOn, PendingAmount: due}
			return nil
		}
		correction, err := uc.Corrections.GetCorrectionByID(tx, in.GymID, in.CorrectionID, true)
		if err != nil {
			return err
		}
		due, err := uc.Corrections.PendingAmount(tx, in.GymID, in.CorrectionID, true)
		if err != nil {
			return err
		}
		if due <= 0 {
			return sharedDomain.NewBusinessError(billingErrors.ErrPendingRefundNotFound, "")
		}
		paymentID, err := uc.correctionRootPaymentID(tx, correction)
		if err != nil {
			return err
		}
		parent, err := uc.Payments.GetByID(tx, paymentID)
		if err != nil {
			return err
		}
		refundPayment, err := uc.createOvercollectionSettlement(ctx, tx, correction, parent, due, in.Method, in.CashDrawerID, in.PaymentDate, in.IdempotencyKey, now)
		if err != nil {
			return err
		}
		out = SettlePendingRefundOutput{RefundID: refundPayment.ID, Amount: due, PaymentMethod: in.Method, RefundedOn: in.PaymentDate, PendingAmount: 0}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// correctionRootPaymentID normally resolves through the live sale. An annulled
// capture-error sale is intentionally tombstoned, though, while its pending
// physical return may be settled later. The immutable correction snapshot is
// therefore the durable link to the original payment for that one case.
func (uc *CorrectSale) correctionRootPaymentID(tx sharedDomain.Transaction, correction *correctionDomain.Correction) (uuid.UUID, error) {
	sale, err := uc.Sales.GetByID(tx, correction.SaleID)
	if err == nil {
		return sale.PaymentID, nil
	}
	if !errors.Is(err, billingErrors.ErrSaleNotFound) || correction.CorrectionType != correctionDomain.TypeAnnul {
		return uuid.Nil, err
	}
	var snapshot struct {
		PaymentID uuid.UUID `json:"payment_id"`
	}
	if json.Unmarshal(correction.BeforeSnapshot, &snapshot) != nil || snapshot.PaymentID == uuid.Nil {
		return uuid.Nil, sharedDomain.NewUnexpectedError(errors.New("annulled sale correction is missing its root payment reference"))
	}
	return snapshot.PaymentID, nil
}

func correctionSnapshot(sale *saleDomain.Sale, items []*saleDomain.SaleItem, payment *paymentDomain.Payment) (json.RawMessage, error) {
	type line struct {
		SaleItemID     string `json:"sale_item_id"`
		ProductID      string `json:"product_id"`
		ProductName    string `json:"product_name"`
		UnitPriceCents int64  `json:"unit_price_cents"`
		UnitCostCents  *int64 `json:"unit_cost_cents,omitempty"`
		Quantity       int    `json:"quantity"`
		LineTotalCents int64  `json:"line_total_cents"`
	}
	rows := make([]line, 0, len(items))
	for _, item := range items {
		if item.DeletedAt != nil {
			continue
		}
		row := line{SaleItemID: item.ID.String(), ProductID: item.ProductID.String(), ProductName: item.ProductNameSnapshot, UnitPriceCents: cents(item.UnitPriceSnapshot), Quantity: item.Quantity, LineTotalCents: cents(item.LineTotal)}
		if item.UnitCostSnapshot != nil {
			v := cents(*item.UnitCostSnapshot)
			row.UnitCostCents = &v
		}
		rows = append(rows, row)
	}
	return json.Marshal(map[string]any{"sale_id": sale.ID, "payment_id": sale.PaymentID, "correction_version": sale.CorrectionVersion,
		"annulled": sale.DeletedAt != nil, "payment_tombstoned": payment.DeletedAt != nil,
		"subtotal_cents": cents(sale.Subtotal), "discount_cents": cents(sale.Discount), "total_cents": cents(sale.Total),
		"payment_amount_cents": cents(payment.Amount), "recognized_amount_cents": cents(payment.RecognizedAmount), "lines": rows})
}
func cloneSaleItem(x *saleDomain.SaleItem) *saleDomain.SaleItem {
	v := *x
	if x.UnitCostSnapshot != nil {
		c := *x.UnitCostSnapshot
		v.UnitCostSnapshot = &c
	}
	return &v
}
func saleLineLabel(x *saleDomain.SaleItem) string {
	if x.Quantity > 1 {
		return x.ProductNameSnapshot + " ×" + itoa(x.Quantity)
	}
	return x.ProductNameSnapshot
}
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	b := make([]byte, 0, 10)
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
func cents(v float64) int64        { return int64(math.Round(v * 100)) }
func roundMoney(v float64) float64 { return math.Round(v*100) / 100 }
func sameMoney(a, b float64) bool  { return cents(a) == cents(b) }

// proportionalDiscount preserves the effective rate represented by an
// already-materialized discount. Sale does not retain whether that amount
// originated from a percentage promotion or a fixed coupon; treating both
// uniformly avoids inventing a second discount policy at correction time.
func proportionalDiscount(oldSubtotal, oldDiscount, newSubtotal float64) float64 {
	oldSubtotalCents := cents(oldSubtotal)
	oldDiscountCents := cents(oldDiscount)
	newSubtotalCents := cents(newSubtotal)
	if oldSubtotalCents <= 0 || oldDiscountCents <= 0 || newSubtotalCents <= 1 {
		return 0
	}
	// Inputs are positive, so adding half the denominator gives cent-safe
	// round-half-up. Keep at least one cent payable for extreme discounts.
	discountCents := (newSubtotalCents*oldDiscountCents + oldSubtotalCents/2) / oldSubtotalCents
	if discountCents >= newSubtotalCents {
		discountCents = newSubtotalCents - 1
	}
	return float64(discountCents) / 100
}

func validRealRefundMethod(v string) bool {
	return v == paymentDomain.MethodCash || v == paymentDomain.MethodTransfer || v == paymentDomain.MethodCard
}
func moneyStatus(resolution string, due float64) string {
	if due > 0 {
		return "pending_refund"
	}
	if resolution == correctionDomain.RefundExcess {
		return "refunded"
	}
	return "corrected"
}

func correctionMoneyStatus(in CorrectSaleInput, pendingRefund float64) string {
	if pendingRefund > 0 {
		return "pending_refund"
	}
	switch in.IncreaseResolution {
	case correctionDomain.IncreasePending:
		return "additional_pending"
	case correctionDomain.IncreaseAlreadyCollected:
		return "historical_collection_corrected"
	case correctionDomain.IncreaseCollectNow:
		return "collected_now"
	}
	if in.Annul {
		if in.MoneyResolution == correctionDomain.RefundExcess {
			return "annulled_refunded"
		}
		return "annulled"
	}
	return moneyStatus(in.MoneyResolution, pendingRefund)
}

func correctionType(annul bool) string {
	if annul {
		return correctionDomain.TypeAnnul
	}
	return correctionDomain.TypeEdit
}

func correctionMonetaryDelta(increase, excess float64) float64 {
	if increase > 0 {
		return increase
	}
	return excess
}
