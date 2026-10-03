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
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RefundPaymentInput backs UC-022. `Method` is the refund medium ("cash" /
// "transfer" / "card"). When `RevertMembership=true` and the parent payment
// is a membership cobro, the use case calls into members to undo the renewal
// (DA-22.4).
type RefundPaymentInput struct {
	GymID            uuid.UUID
	ActorUserID      uuid.UUID
	ParentPaymentID  uuid.UUID
	Reason           string
	Method           string
	CashDrawerID     *uuid.UUID
	Amount           float64 // optional — defaults to parent.Amount
	PaymentDate      time.Time
	RevertMembership bool
	IdempotencyKey   string
	SaleID           *uuid.UUID
	Items            []refundDomain.ItemInput
	CorrectionID     *uuid.UUID
	Kind             string
	// Prepared financial split for product returns. Amount remains the
	// economic value selected by the caller; cash is only the portion that
	// exceeds outstanding debt.
	MonetaryAmount   float64
	BalanceCancelled float64
}

type RefundPaymentOutput struct {
	RefundID         uuid.UUID
	RefundPaymentID  *uuid.UUID
	RefundFolio      string
	Amount           float64
	BalanceCancelled float64
	Reverted         bool
}

// RefundPreviewOutput makes the distinction between the row the owner clicked
// and the complete obligation explicit. A root payment can have later
// settlements, and a settlement can only return its own still-refundable
// collection. None of these figures is inferred by the client.
type RefundPreviewOutput struct {
	SelectedPaymentID       uuid.UUID
	RootPaymentID           uuid.UUID
	SelectedRefundable      float64
	AggregateCollected      float64
	AggregateRefunded       float64
	AggregateRefundable     float64
	BalancePending          float64
	RevertMembershipTotal   float64
	MembershipRevertAllowed bool
	MembershipRevertReason  string
}

type RefundPayment struct {
	Payments  billingRepo.PaymentRepository
	Folios    *folioSvc.Generator
	MemberSvc *memApp.MemberService
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	// Gyms (opcional) → default de PaymentDate en el día LOCAL del gym
	// (ver gymLocalPaymentDate). Nil = día UTC (tests viejos).
	Gyms        gymRepo.GymRepository
	Refunds     billingRepo.RefundRepository
	Sales       billingRepo.SaleRepository
	SaleItems   billingRepo.SaleItemRepository
	Products    *prodApp.ProductService
	CashDrawers CashDrawerValidator
}

func (uc *RefundPayment) WithCashDrawers(v CashDrawerValidator) *RefundPayment {
	uc.CashDrawers = v
	return uc
}

func (uc *RefundPayment) WithRefunds(r billingRepo.RefundRepository) *RefundPayment {
	uc.Refunds = r
	return uc
}

func (uc *RefundPayment) WithSaleDetails(sales billingRepo.SaleRepository, items billingRepo.SaleItemRepository, products *prodApp.ProductService) *RefundPayment {
	uc.Sales, uc.SaleItems, uc.Products = sales, items, products
	return uc
}

// WithGyms cablea el repo de gyms para anclar el default de PaymentDate
// al día calendario del gym en SU zona horaria.
func (uc *RefundPayment) WithGyms(g gymRepo.GymRepository) *RefundPayment {
	uc.Gyms = g
	return uc
}

func NewRefundPayment(payments billingRepo.PaymentRepository, folios *folioSvc.Generator,
	memberSvc *memApp.MemberService, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *RefundPayment {
	return &RefundPayment{
		Payments: payments, Folios: folios, MemberSvc: memberSvc, UoW: uow, Audit: recorder,
	}
}

// Preview returns the server-authoritative monetary limits for a refund. It is
// deliberately read-only and uses the same aggregate queries as Execute, so a
// UI never guesses from a paginated payment row or silently includes later
// settlements when the owner clicked the original collection.
func (uc *RefundPayment) Preview(ctx context.Context, gymID, selectedPaymentID uuid.UUID) (*RefundPreviewOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	readBalance := func(paymentID uuid.UUID) (billingRepo.RefundBalance, error) {
		if reader, ok := uc.Payments.(billingRepo.PaymentRefundReader); ok {
			return reader.RefundBalance(tx, gymID, paymentID)
		}
		p, getErr := uc.Payments.GetByID(tx, paymentID)
		if getErr != nil {
			return billingRepo.RefundBalance{}, getErr
		}
		if p == nil {
			return billingRepo.RefundBalance{}, sharedDomain.NewBusinessError(billingErrors.ErrPaymentNotFound, "")
		}
		return billingRepo.RefundBalance{
			Root: p, Collected: p.Amount, Recognized: p.RecognizedAmount,
			Refundable: p.Amount, SourceRefundable: p.Amount,
		}, nil
	}

	selected, err := readBalance(selectedPaymentID)
	if err != nil {
		return nil, err
	}
	if selected.Root == nil || selected.Root.GymID != gymID {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
	}
	if !selected.Root.IsRefundable() {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrCannotRefundNonPayment, "")
	}

	rootID := selectedPaymentID
	if selected.Root.Concept == paymentDomain.ConceptBalanceSettlement {
		if selected.Root.ParentPaymentID == nil {
			return nil, sharedDomain.NewBusinessError(billingErrors.ErrCannotRefundNonPayment, "")
		}
		rootID = *selected.Root.ParentPaymentID
	}
	aggregate := selected
	if rootID != selectedPaymentID {
		aggregate, err = readBalance(rootID)
		if err != nil {
			return nil, err
		}
	}
	if aggregate.Root == nil || aggregate.Root.GymID != gymID ||
		(aggregate.Root.Concept != paymentDomain.ConceptMembership &&
			aggregate.Root.Concept != paymentDomain.ConceptProduct &&
			aggregate.Root.Concept != paymentDomain.ConceptOther) {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrCannotRefundNonPayment, "")
	}

	selectedRefundable := math.Min(selected.SourceRefundable, aggregate.Refundable)
	if selectedRefundable < 0 {
		selectedRefundable = 0
	}
	// Automatic membership reversal remains fail-closed until the payment
	// records every side effect (enrollment, maintenance, promotion uses,
	// extra days and gifted memberships). Reverting only the membership row
	// would leave a financially plausible but operationally corrupted state.
	revertReason := ""
	if aggregate.Root.Concept == paymentDomain.ConceptMembership {
		revertReason = billingErrors.ErrMembershipRevertUnsafe.Error()
	}
	return &RefundPreviewOutput{
		SelectedPaymentID:       selectedPaymentID,
		RootPaymentID:           rootID,
		SelectedRefundable:      roundMoney(selectedRefundable),
		AggregateCollected:      roundMoney(aggregate.Collected),
		AggregateRefunded:       roundMoney(aggregate.Refunded),
		AggregateRefundable:     roundMoney(aggregate.Refundable),
		BalancePending:          roundMoney(aggregate.Root.BalancePending),
		RevertMembershipTotal:   roundMoney(aggregate.Refundable + aggregate.Root.BalancePending),
		MembershipRevertAllowed: false,
		MembershipRevertReason:  revertReason,
	}, nil
}

func (uc *RefundPayment) Execute(ctx context.Context, in RefundPaymentInput) (*RefundPaymentOutput, error) {
	if strings.TrimSpace(in.Reason) == "" {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrRefundReasonRequired)
	}
	key, err := validateRefundCommandKey(in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	in.IdempotencyKey = key
	fingerprint, err := refundCommandFingerprint(in)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := time.Now().UTC()

	var out RefundPaymentOutput
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		refundDay, dateErr := resolveMonetaryDate(tx, uc.Gyms, in.GymID, in.PaymentDate, now)
		if dateErr != nil {
			return dateErr
		}
		in.PaymentDate = refundDay
		if uc.Refunds != nil {
			existing, getErr := uc.Refunds.GetByIdempotencyKey(tx, in.GymID, key)
			if getErr != nil {
				return sharedDomain.NewUnexpectedError(getErr)
			}
			if existing != nil {
				if existing.IdempotencyFingerprint != fingerprint {
					return sharedDomain.NewBusinessError(billingErrors.ErrRefundIdempotencyConflict, "")
				}
				if len(existing.IdempotencyResult) == 0 {
					return sharedDomain.NewUnexpectedError(errors.New("keyed refund has no replay result"))
				}
				if unmarshalErr := json.Unmarshal(existing.IdempotencyResult, &out); unmarshalErr != nil {
					return sharedDomain.NewUnexpectedError(unmarshalErr)
				}
				return nil
			}
		}
		if in.RevertMembership {
			// See Preview: until every enrollment/promotion side effect is linked
			// to this payment, a partial automatic reversal is more dangerous than
			// an explicit manual membership adjustment. This check intentionally
			// follows replay lookup so reusing a committed key with the checkbox
			// changed is reported as an idempotency conflict.
			return sharedDomain.NewBusinessError(billingErrors.ErrMembershipRevertUnsafe, "")
		}
		if err := validateRequestedCashDrawer(uc.CashDrawers, tx, in.GymID, in.Method, in.CashDrawerID); err != nil {
			return err
		}
		var balance billingRepo.RefundBalance
		var balanceErr error
		if locker, ok := uc.Payments.(billingRepo.PaymentRefundLocker); ok {
			balance, balanceErr = locker.RefundBalanceForUpdate(tx, in.GymID, in.ParentPaymentID)
		} else {
			var parent *paymentDomain.Payment
			parent, balanceErr = uc.Payments.GetByID(tx, in.ParentPaymentID)
			balance = billingRepo.RefundBalance{Root: parent}
			if parent != nil {
				balance.Collected, balance.Recognized, balance.Refundable = parent.Amount, parent.RecognizedAmount, parent.Amount
				balance.SourceRefundable = parent.Amount
			}
		}
		if balanceErr != nil {
			return balanceErr
		}
		sourcePayment := balance.Root
		if sourcePayment.GymID != in.GymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}

		// A settlement is a real collection that may itself be refunded. The
		// negative payment points to that settlement for traceability, while the
		// original obligation remains the aggregate root whose debt is reopened.
		rootPayment := sourcePayment
		rootBalance := balance
		isSettlementRefund := sourcePayment.Concept == paymentDomain.ConceptBalanceSettlement
		if isSettlementRefund {
			if sourcePayment.ParentPaymentID == nil || in.RevertMembership {
				return sharedDomain.NewBusinessError(billingErrors.ErrCannotRefundNonPayment, "")
			}
			if locker, ok := uc.Payments.(billingRepo.PaymentRefundLocker); ok {
				rootBalance, balanceErr = locker.RefundBalanceForUpdate(tx, in.GymID, *sourcePayment.ParentPaymentID)
			} else {
				rootPayment, balanceErr = uc.Payments.GetByID(tx, *sourcePayment.ParentPaymentID)
				rootBalance = billingRepo.RefundBalance{Root: rootPayment}
				if rootPayment != nil {
					rootBalance.Collected = rootPayment.Amount
					rootBalance.Recognized = rootPayment.RecognizedAmount
					rootBalance.Refundable = rootPayment.Amount
					rootBalance.SourceRefundable = rootPayment.Amount
				}
			}
			if balanceErr != nil {
				return balanceErr
			}
			rootPayment = rootBalance.Root
		}
		if rootPayment == nil || rootPayment.GymID != in.GymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}
		if err := validateMonetaryChronology(in.PaymentDate, sourcePayment.PaymentDate); err != nil {
			return err
		}
		if !sourcePayment.IsRefundable() || (rootPayment.Concept != paymentDomain.ConceptMembership &&
			rootPayment.Concept != paymentDomain.ConceptProduct && rootPayment.Concept != paymentDomain.ConceptOther) {
			return sharedDomain.NewBusinessError(billingErrors.ErrCannotRefundNonPayment, "")
		}

		if !isSettlementRefund && rootPayment.Concept == paymentDomain.ConceptProduct && in.SaleID == nil && uc.Sales != nil {
			sale, saleErr := uc.Sales.GetByPaymentID(tx, rootPayment.ID)
			if saleErr != nil {
				return saleErr
			}
			id := sale.ID
			in.SaleID = &id
		}
		if in.SaleID != nil {
			prepared, saleErr := uc.prepareSaleRefund(tx, in, rootPayment)
			if saleErr != nil {
				return saleErr
			}
			in = prepared
		}
		economicAmount, monetaryAmount := in.Amount, in.MonetaryAmount
		if in.SaleID == nil {
			if economicAmount <= 0 {
				// Never infer an aggregate refund from a row click. The caller must
				// confirm the previewed physical amount explicitly.
				return sharedDomain.NewValidationError(billingErrors.ErrAmountInvalid)
			}
			monetaryAmount = economicAmount
			if in.RevertMembership {
				if rootPayment.Concept != paymentDomain.ConceptMembership || rootPayment.MemberID == nil ||
					!sameMoney(monetaryAmount, rootBalance.Refundable) {
					return sharedDomain.NewBusinessError(billingErrors.ErrRefundExceedsCollected, "")
				}
				// Reverting the service cancels the entire obligation: return every
				// collected peso and remove the still-uncollected debt in the same
				// transaction. A partial cash refund may only keep service active.
				in.BalanceCancelled = rootPayment.BalancePending
				economicAmount = roundMoney(monetaryAmount + in.BalanceCancelled)
			}
		}
		refundLimit := rootBalance.Refundable
		if in.SaleID == nil && !in.RevertMembership {
			refundLimit = math.Min(balance.SourceRefundable, rootBalance.Refundable)
		}
		if economicAmount <= 0 || monetaryAmount < 0 || monetaryAmount > refundLimit {
			return sharedDomain.NewBusinessError(billingErrors.ErrRefundExceedsCollected, "")
		}
		if monetaryAmount == 0 {
			in.Method = ""
		}

		var refundPayment *paymentDomain.Payment
		if monetaryAmount > 0 {
			folio, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptRefund)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			refundPayment, err = paymentDomain.NewRefundPaymentWithin(
				uuid.New(), in.GymID, in.ActorUserID,
				sourcePayment, folio, monetaryAmount, refundLimit, in.Method, in.Reason, in.PaymentDate, now,
			)
			if err != nil {
				if err == billingErrors.ErrCannotRefundNonPayment || err == billingErrors.ErrRefundReasonRequired {
					return sharedDomain.NewBusinessError(err, "")
				}
				return sharedDomain.NewValidationError(err)
			}
			if in.Method == paymentDomain.MethodCash {
				drawerID := in.GymID
				if in.CashDrawerID != nil {
					drawerID = *in.CashDrawerID
				}
				refundPayment.WithCashDrawer(drawerID)
			}
		}

		if isSettlementRefund {
			if _, err := rootPayment.ReopenBalance(monetaryAmount, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
		} else if in.BalanceCancelled > 0 {
			if _, err := rootPayment.CancelBalance(in.BalanceCancelled, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
		} else {
			// Force the root upsert before the dependent refund payment reaches
			// the offline queue/cloud FK.
			rootPayment.Touch(now)
		}
		if _, err := uc.Payments.Update(tx, rootPayment); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if refundPayment != nil {
			if _, err := uc.Payments.Create(tx, refundPayment); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}

		refundID := uuid.Nil
		var refundPaymentID *uuid.UUID
		if refundPayment != nil {
			id := refundPayment.ID
			refundPaymentID = &id
			refundID = id
		}
		out = RefundPaymentOutput{RefundID: refundID, RefundPaymentID: refundPaymentID,
			Amount: monetaryAmount, BalanceCancelled: in.BalanceCancelled}
		if refundPayment != nil {
			out.RefundFolio = refundPayment.Folio
		}
		if uc.Refunds != nil {
			aggregateID := uuid.New()
			out.RefundID = aggregateID
			result, marshalErr := json.Marshal(out)
			if marshalErr != nil {
				return sharedDomain.NewUnexpectedError(marshalErr)
			}
			aggregate, err := refundDomain.New(refundDomain.Input{
				ID: aggregateID, GymID: in.GymID, RootPaymentID: rootPayment.ID,
				RefundPaymentID: refundPaymentID, CreatedBy: in.ActorUserID, SaleID: in.SaleID,
				Amount: monetaryAmount, BalanceCancelled: in.BalanceCancelled,
				Method: in.Method, RefundedOn: in.PaymentDate,
				Reason: in.Reason, IdempotencyKey: key, Items: in.Items, Now: now,
				CorrectionID: in.CorrectionID, Kind: in.Kind,
				IdempotencyFingerprint: fingerprint, IdempotencyResult: result,
			})
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if _, err := uc.Refunds.Create(tx, aggregate); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			refundID = aggregate.ID
			if in.SaleID != nil {
				lines, err := uc.SaleItems.ListBySale(tx, *in.SaleID)
				if err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				for _, item := range aggregate.Items {
					if item.Disposition != refundDomain.ReturnedToStock {
						continue
					}
					line := findSaleItemByID(lines, item.SaleItemID)
					if line == nil || uc.Products == nil {
						return sharedDomain.NewUnexpectedError(errors.New("sale refund stock service is not configured"))
					}
					if err := uc.Products.IncrementForRefund(ctx, tx, prodApp.IncrementForRefundInput{
						GymID: in.GymID, ProductID: line.ProductID, Quantity: item.Quantity,
						OperatorID: in.ActorUserID, SaleItemID: line.ID,
					}, now); err != nil {
						return err
					}
				}
			}
		}

		// Optional: revert membership renewal (DA-22.4). Only meaningful when
		// the original payment was a membership cobro and we have a member.
		reverted := false
		if in.RevertMembership && rootPayment.Concept == paymentDomain.ConceptMembership && rootPayment.MemberID != nil {
			membershipID := uuid.Nil
			if rootPayment.MembershipID != nil {
				membershipID = *rootPayment.MembershipID
			}
			if _, err := uc.MemberSvc.RevertMembershipFromPayment(ctx, tx,
				memApp.RevertMembershipFromPaymentInput{MemberID: *rootPayment.MemberID, MembershipID: membershipID}, now); err != nil {
				return err
			}
			reverted = true
		}

		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "refunds",
			EntityID:    refundID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"root_payment_id":   rootPayment.ID,
				"source_payment_id": sourcePayment.ID,
				"amount":            monetaryAmount,
				"balance_cancelled": in.BalanceCancelled,
				"reason":            in.Reason,
				"reverted_renewal":  reverted,
				"concept":           paymentDomain.ConceptRefund,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})

		out.RefundID = refundID
		out.Reverted = reverted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (uc *RefundPayment) prepareSaleRefund(tx sharedDomain.Transaction, in RefundPaymentInput, parent *paymentDomain.Payment) (RefundPaymentInput, error) {
	if uc.Sales == nil || uc.SaleItems == nil || uc.Refunds == nil {
		return in, sharedDomain.NewUnexpectedError(errors.New("sale refund repositories are not configured"))
	}
	sale, err := uc.Sales.GetByID(tx, *in.SaleID)
	if err != nil {
		return in, err
	}
	if sale.GymID != in.GymID || sale.PaymentID != parent.ID {
		return in, sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
	}
	lines, err := uc.SaleItems.ListBySale(tx, sale.ID)
	if err != nil {
		return in, sharedDomain.NewUnexpectedError(err)
	}
	used, err := uc.Refunds.RefundedQuantitiesBySale(tx, in.GymID, sale.ID)
	if err != nil {
		return in, sharedDomain.NewUnexpectedError(err)
	}
	byID := make(map[uuid.UUID]*saleDomain.SaleItem, len(lines))
	for _, line := range lines {
		byID[line.ID] = line
	}
	lineAllocations := allocatedSaleLineTotals(sale.Subtotal, sale.Total, lines)
	items := append([]refundDomain.ItemInput(nil), in.Items...)
	if len(items) == 0 {
		return in, sharedDomain.NewValidationError(billingErrors.ErrRefundDispositionInvalid)
	}
	seen := make(map[uuid.UUID]bool, len(items))
	var total float64
	for i := range items {
		line := byID[items[i].SaleItemID]
		if line == nil || seen[items[i].SaleItemID] || items[i].Quantity <= 0 ||
			items[i].Quantity+used[items[i].SaleItemID] > line.Quantity {
			return in, sharedDomain.NewBusinessError(billingErrors.ErrRefundQuantityExceeded, "")
		}
		seen[items[i].SaleItemID] = true
		// Amount is authoritative on the server. A client may send the legacy
		// field, but it cannot discount a return or manufacture a larger refund.
		// Cumulative allocation makes successive partials telescope exactly.
		items[i].Amount = allocatedLineDelta(
			lineAllocations[line.ID], used[line.ID], items[i].Quantity, line.Quantity,
		)
		if items[i].Amount <= 0 {
			return in, sharedDomain.NewValidationError(billingErrors.ErrAmountInvalid)
		}
		total += items[i].Amount
	}
	total = math.Round(total*100) / 100
	if in.Amount <= 0 {
		in.Amount = total
	}
	if math.Round(in.Amount*100) != math.Round(total*100) {
		return in, sharedDomain.NewValidationError(billingErrors.ErrAmountInvalid)
	}
	in.Items = items
	in.BalanceCancelled = math.Round(math.Min(parent.BalancePending, total)*100) / 100
	in.MonetaryAmount = math.Round((total-in.BalanceCancelled)*100) / 100
	return in, nil
}

// allocatedSaleLineTotals distributes the sale's post-discount total across
// lines in stable UUID order. Cumulative rounding makes all complete line
// allocations sum to sale.Total, even when a global discount leaves a cent
// that cannot be split evenly.
func allocatedSaleLineTotals(subtotal, total float64, lines []*saleDomain.SaleItem) map[uuid.UUID]float64 {
	out := make(map[uuid.UUID]float64, len(lines))
	if subtotal <= 0 || total <= 0 || len(lines) == 0 {
		return out
	}
	ordered := append([]*saleDomain.SaleItem(nil), lines...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID.String() < ordered[j].ID.String() })
	subtotalCents, totalCents := cents(subtotal), cents(total)
	var runningSubtotal, previousAllocation int64
	for i, line := range ordered {
		runningSubtotal += cents(line.LineTotal)
		cumulativeAllocation := totalCents
		if i < len(ordered)-1 {
			cumulativeAllocation = roundedRatio(runningSubtotal, totalCents, subtotalCents)
			if cumulativeAllocation < previousAllocation {
				cumulativeAllocation = previousAllocation
			}
			if cumulativeAllocation > totalCents {
				cumulativeAllocation = totalCents
			}
		}
		out[line.ID] = float64(cumulativeAllocation-previousAllocation) / 100
		previousAllocation = cumulativeAllocation
	}
	return out
}

// allocatedLineDelta returns alloc(already+new)-alloc(already), rather than
// rounding each partial return independently.
func allocatedLineDelta(lineTotal float64, already, quantity, fullQuantity int) float64 {
	if lineTotal <= 0 || already < 0 || quantity <= 0 || fullQuantity <= 0 || already+quantity > fullQuantity {
		return 0
	}
	lineCents := cents(lineTotal)
	before := roundedRatio(int64(already), lineCents, int64(fullQuantity))
	after := roundedRatio(int64(already+quantity), lineCents, int64(fullQuantity))
	return float64(after-before) / 100
}

func roundedRatio(value, multiplier, divisor int64) int64 {
	if value <= 0 || multiplier <= 0 || divisor <= 0 {
		return 0
	}
	return int64(math.Floor(float64(value)*float64(multiplier)/float64(divisor) + 0.5))
}

func findSaleItemByID(items []*saleDomain.SaleItem, id uuid.UUID) *saleDomain.SaleItem {
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	return nil
}
