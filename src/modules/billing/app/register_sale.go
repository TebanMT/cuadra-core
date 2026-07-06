package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	saleDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/sale"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	promoApp "github.com/cuadra/cuadra-core/src/modules/promotions/app"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RegisterSaleInput backs UC-025. The cart is a list of (product_id, quantity)
// — price/name are looked up server-side from the current Product so the
// frontend can't fake them. `MemberID` is optional (DA-25.3).
//
// `Paid` habilita el caso "fiado": cuando es no-nil y menor que el total
// post-discount, el faltante se persiste como balance_pending en el
// Payment y se liquida después por el flujo estándar de abonos
// (POST /payments/:id/settle). Si es nil, se cobra el total — caso default.
// Reglas adicionales: requiere MemberID (no se fía a walk-ins), y el valor
// debe ser > 0.
type RegisterSaleInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Method      string
	MemberID    *uuid.UUID
	Discount    float64 // optional, MVP usually 0
	Paid        *float64
	PaymentDate time.Time
	Notes       *string
	Items       []SaleLineInput
	// Promotion opcional. En ventas sólo aplican percent / fixed_amount.
	// extra_days y companion_memberships son no-op silencioso (su efecto
	// no tiene sentido en una venta de producto). free_enrollment también
	// es no-op (no hay enrollment en ventas).
	Promotion *PromotionApply
}

type SaleLineInput struct {
	ProductID uuid.UUID
	Quantity  int
}

type RegisterSaleOutput struct {
	SaleID         uuid.UUID
	PaymentID      uuid.UUID
	Folio          string
	Subtotal       float64
	Discount       float64
	Total          float64
	Paid           float64 // monto cobrado (== Total cuando no hay fiado)
	BalancePending float64 // queda > 0 en ventas a crédito
	Items          []SaleItemOutput
	// Datos de promo aplicada — vacíos cuando no hubo promo.
	PromotionAppliedID *uuid.UUID
	PromotionName      string
	PromotionKind      string
}

type SaleItemOutput struct {
	ProductID   uuid.UUID
	ProductName string
	UnitPrice   float64
	Quantity    int
	LineTotal   float64
	StockAfter  int
	SaleItemID  uuid.UUID
	StockMoveID uuid.UUID
}

// RegisterSale orchestrates UC-025 inside a single UoW.Command:
//
//  1. Validate carrito non-empty + sane qty.
//  2. For each item: ProductService.DecrementForSale (reads product, checks
//     stock, decrements + writes 'sale' stock_movement). Cross-BC seam.
//  3. Compute subtotal/total from snapshots.
//  4. Mint PRD-NNNNNN folio.
//  5. Create Payment(concept='product').
//  6. Create Sale linking payment_id.
//  7. Create sale_items (snapshots).
//  8. Backfill stock_movement.sale_item_id (we needed sale_items to exist
//     first; the products service used uuid.New() upfront — we pass the same
//     id through so consistency holds without a backfill query).
//  9. Audit + emit SaleCompleted event.
type RegisterSale struct {
	Payments   billingRepo.PaymentRepository
	Sales      billingRepo.SaleRepository
	SaleItems  billingRepo.SaleItemRepository
	Folios     *folioSvc.Generator
	Products   *prodApp.ProductService
	Members    memRepo.MemberRepository
	UoW        sharedDomain.UnitOfWork
	Audit      audit.Recorder
	Publisher  EventPublisher
	Promotions *promoApp.ApplyPromotion // opcional; nil → no se aplican promos
	// Gyms (opcional) → default de PaymentDate en el día LOCAL del gym
	// (ver gymLocalPaymentDate). Nil = día UTC (tests viejos).
	Gyms gymRepo.GymRepository
}

// WithPromotions engancha el use case de promociones.
func (uc *RegisterSale) WithPromotions(p *promoApp.ApplyPromotion) *RegisterSale {
	uc.Promotions = p
	return uc
}

// WithGyms cablea el repo de gyms para anclar el default de PaymentDate
// al día calendario del gym en SU zona horaria.
func (uc *RegisterSale) WithGyms(g gymRepo.GymRepository) *RegisterSale {
	uc.Gyms = g
	return uc
}

func NewRegisterSale(
	payments billingRepo.PaymentRepository,
	sales billingRepo.SaleRepository,
	saleItems billingRepo.SaleItemRepository,
	folios *folioSvc.Generator,
	productSvc *prodApp.ProductService,
	members memRepo.MemberRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
	publisher EventPublisher,
) *RegisterSale {
	if publisher == nil {
		publisher = NoopPublisher{}
	}
	return &RegisterSale{
		Payments: payments, Sales: sales, SaleItems: saleItems,
		Folios: folios, Products: productSvc, Members: members,
		UoW: uow, Audit: recorder, Publisher: publisher,
	}
}

func (uc *RegisterSale) Execute(ctx context.Context, in RegisterSaleInput) (*RegisterSaleOutput, error) {
	if len(in.Items) == 0 {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrSaleEmpty)
	}
	if in.Method == "" {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodMissing)
	}
	now := time.Now().UTC()

	// Reserve sale_item ids upfront so we can pass them to the products service
	// (the stock_movement row references sale_item_id; we want a single tx that
	// inserts sale_items + stock_movements with FK satisfied immediately).
	saleItemIDs := make([]uuid.UUID, len(in.Items))
	for i := range in.Items {
		saleItemIDs[i] = uuid.New()
	}

	var (
		out RegisterSaleOutput
		evt PaymentCompletedEvent
	)
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// Default de fecha: el día LOCAL del gym, no el día UTC — una venta
		// a las 10 PM pertenece a la caja del día en curso.
		if in.PaymentDate.IsZero() {
			in.PaymentDate = gymLocalPaymentDate(tx, uc.Gyms, in.GymID, now)
		}

		// Optional member sanity check.
		if in.MemberID != nil {
			m, err := uc.Members.GetByID(tx, *in.MemberID)
			if err != nil {
				return err
			}
			if m.GymID != in.GymID {
				return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
			}
		}

		// 1+2. Decrement stock per line + collect snapshots.
		snapshots := make([]saleDomain.ItemInput, 0, len(in.Items))
		stockAfter := make([]int, 0, len(in.Items))
		for i, item := range in.Items {
			if item.Quantity <= 0 {
				return sharedDomain.NewValidationError(billingErrors.ErrSaleItemQuantityInvalid)
			}
			res, err := uc.Products.DecrementForSale(ctx, tx, prodApp.DecrementForSaleInput{
				GymID:      in.GymID,
				ProductID:  item.ProductID,
				Quantity:   item.Quantity,
				OperatorID: in.ActorUserID,
				SaleItemID: saleItemIDs[i],
			}, now)
			if err != nil {
				return err
			}
			snapshots = append(snapshots, saleDomain.ItemInput{
				ProductID:           res.Product.ID,
				ProductNameSnapshot: res.Product.Name,
				UnitPriceSnapshot:   res.Product.Price,
				Quantity:            item.Quantity,
			})
			stockAfter = append(stockAfter, res.Product.Stock)
		}

		// 3+4. Mint folio.
		folio, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptProduct)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 4.5 Promo: validar y resolver el discount antes de armar el
		//     Sale. En ventas sólo aplican percent y fixed_amount; los
		//     demás kinds son no-op (Calculator devuelve 0). Stacking
		//     manual+promo prohibido — coherencia con membership.
		discount := in.Discount
		paymentIDReserved := uuid.New()
		var promoResult *promoApp.ApplyPromotionResult
		if in.Promotion != nil && uc.Promotions != nil {
			if discount > 0 {
				return sharedDomain.NewBusinessError(billingErrors.ErrDiscountTooLarge, "no puedes combinar descuento manual con promoción")
			}
			// Computar subtotal previo desde snapshots para que el calculator
			// reciba el monto real (Sale.NewSale lo recalcula igual).
			var subtotal float64
			for _, it := range snapshots {
				subtotal += it.UnitPriceSnapshot * float64(it.Quantity)
			}
			res, err := uc.Promotions.Resolve(ctx, tx, promoApp.ApplyPromotionInput{
				GymID:       in.GymID,
				ActorUserID: in.ActorUserID,
				PromotionID: in.Promotion.PromotionID,
				Code:        in.Promotion.Code,
				PaymentID:   paymentIDReserved,
				Target:      promoDomain.AppliesToSale,
				Subtotal:    subtotal,
				MemberID:    in.MemberID,
				Notes:       in.Promotion.Notes,
				Today:       in.PaymentDate,
			}, now)
			if err != nil {
				return err
			}
			promoResult = res
			if res.Discount > 0 {
				discount = res.Discount
			}
		}

		// 5. Build the Sale aggregate (and its items) so we know the totals.
		saleID := uuid.New()
		// Override the items' generated UUIDs with our pre-reserved ids so the
		// stock_movement.sale_item_id FK matches (NewSale assigns uuid.New() —
		// we stomp it back to the reserved value).
		s, err := saleDomain.NewSale(saleID, saleDomain.NewSaleInput{
			GymID:     in.GymID,
			PaymentID: uuid.Nil, // payment id known after payment insert
			MemberID:  in.MemberID,
			Discount:  discount,
			Items:     snapshots,
		}, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		for i := range s.Items {
			s.Items[i].ID = saleItemIDs[i]
		}

		// 6. Create the Payment row first — Sale.PaymentID has a NOT NULL FK.
		// `paid` default = total (cobro completo). Si el operador eligió
		// fiado, in.Paid trae el monto efectivamente cobrado y el resto
		// queda como balance_pending. Fiado exige socio asociado: sin
		// member_id no hay a quién cobrarle el saldo después.
		paid := s.Total
		if in.Paid != nil {
			if in.MemberID == nil {
				return sharedDomain.NewBusinessError(billingErrors.ErrCreditRequiresMember, "")
			}
			paid = *in.Paid
		}
		p, err := paymentDomain.NewProductSalePayment(
			paymentIDReserved, in.GymID, in.ActorUserID, in.MemberID, folio,
			s.Total, paid, in.Method, in.PaymentDate, now, in.Notes,
		)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		// Desglose por producto vendido. Cuando hay cantidad > 1 lo
		// reflejamos en el label ("Proteína 1kg ×2"); el amount es el
		// line total (qty × unit_price), que ya viene calculado en el
		// SaleItem. Si hay descuento global se anota como línea negativa
		// al final para que el subtotal cuadre con p.Amount.
		breakdown := make([]paymentDomain.BreakdownLine, 0, len(s.Items)+1)
		for _, si := range s.Items {
			label := si.ProductNameSnapshot
			if si.Quantity > 1 {
				label = fmt.Sprintf("%s ×%d", label, si.Quantity)
			}
			breakdown = append(breakdown, paymentDomain.BreakdownLine{
				Label: label, Amount: si.LineTotal,
			})
		}
		if s.Discount > 0 {
			breakdown = append(breakdown, paymentDomain.BreakdownLine{
				Label: "Descuento", Amount: -s.Discount,
			})
		}
		p.SetBreakdown(breakdown)
		if _, err := uc.Payments.Create(tx, p); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 7. Persist Sale + sale_items.
		s.PaymentID = p.ID
		if _, err := uc.Sales.Create(tx, s); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err := uc.SaleItems.CreateMany(tx, s.Items); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 7.5 Promo: persistir applied_promotion ahora que el payment existe.
		if promoResult != nil {
			if err := uc.Promotions.Persist(ctx, tx, promoResult, p.ID, now); err != nil {
				return err
			}
		}

		// 8. Audit + event.
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "sales",
			EntityID:    s.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"folio":           p.Folio,
				"payment_id":      p.ID,
				"member_id":       in.MemberID,
				"items":           len(s.Items),
				"subtotal":        s.Subtotal,
				"discount":        s.Discount,
				"total":           s.Total,
				"paid":            p.Amount,
				"balance_pending": p.BalancePending,
				"method":          p.PaymentMethod,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		evt = PaymentCompletedEvent{
			GymID:      in.GymID,
			PaymentID:  p.ID,
			MemberID:   in.MemberID,
			Concept:    p.Concept,
			Amount:     p.Amount,
			Folio:      p.Folio,
			OperatorID: in.ActorUserID,
		}

		items := make([]SaleItemOutput, len(s.Items))
		for i, si := range s.Items {
			items[i] = SaleItemOutput{
				ProductID:   si.ProductID,
				ProductName: si.ProductNameSnapshot,
				UnitPrice:   si.UnitPriceSnapshot,
				Quantity:    si.Quantity,
				LineTotal:   si.LineTotal,
				StockAfter:  stockAfter[i],
				SaleItemID:  si.ID,
			}
		}
		out = RegisterSaleOutput{
			SaleID:         s.ID,
			PaymentID:      p.ID,
			Folio:          p.Folio,
			Subtotal:       s.Subtotal,
			Discount:       s.Discount,
			Total:          s.Total,
			Paid:           p.Amount,
			BalancePending: p.BalancePending,
			Items:          items,
		}
		if promoResult != nil {
			id := promoResult.AppliedID
			out.PromotionAppliedID = &id
			out.PromotionName = promoResult.Promotion.Name
			out.PromotionKind = promoResult.Promotion.Kind
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	uc.Publisher.PublishPaymentCompleted(ctx, evt)
	return &out, nil
}
