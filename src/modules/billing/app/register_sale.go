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
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RegisterSaleInput backs UC-025. The cart is a list of (product_id, quantity)
// — price/name are looked up server-side from the current Product so the
// frontend can't fake them. `MemberID` is optional (DA-25.3).
type RegisterSaleInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Method      string
	MemberID    *uuid.UUID
	Discount    float64 // optional, MVP usually 0
	PaymentDate time.Time
	Notes       *string
	Items       []SaleLineInput
}

type SaleLineInput struct {
	ProductID uuid.UUID
	Quantity  int
}

type RegisterSaleOutput struct {
	SaleID    uuid.UUID
	PaymentID uuid.UUID
	Folio     string
	Subtotal  float64
	Discount  float64
	Total     float64
	Items     []SaleItemOutput
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
	Payments  billingRepo.PaymentRepository
	Sales     billingRepo.SaleRepository
	SaleItems billingRepo.SaleItemRepository
	Folios    *folioSvc.Generator
	Products  *prodApp.ProductService
	Members   memRepo.MemberRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	Publisher EventPublisher
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
	if in.PaymentDate.IsZero() {
		in.PaymentDate = now
	}

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

		// 5. Build the Sale aggregate (and its items) so we know the totals.
		saleID := uuid.New()
		// Override the items' generated UUIDs with our pre-reserved ids so the
		// stock_movement.sale_item_id FK matches (NewSale assigns uuid.New() —
		// we stomp it back to the reserved value).
		s, err := saleDomain.NewSale(saleID, saleDomain.NewSaleInput{
			GymID:     in.GymID,
			PaymentID: uuid.Nil, // payment id known after payment insert
			MemberID:  in.MemberID,
			Discount:  in.Discount,
			Items:     snapshots,
		}, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		for i := range s.Items {
			s.Items[i].ID = saleItemIDs[i]
		}

		// 6. Create the Payment row first — Sale.PaymentID has a NOT NULL FK.
		p, err := paymentDomain.NewProductSalePayment(
			uuid.New(), in.GymID, in.ActorUserID, in.MemberID, folio,
			s.Total, in.Method, in.PaymentDate, now, in.Notes,
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

		// 8. Audit + event.
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "sales",
			EntityID:    s.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"folio":      p.Folio,
				"payment_id": p.ID,
				"member_id":  in.MemberID,
				"items":      len(s.Items),
				"subtotal":   s.Subtotal,
				"discount":   s.Discount,
				"total":      s.Total,
				"method":     p.PaymentMethod,
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
			SaleID:    s.ID,
			PaymentID: p.ID,
			Folio:     p.Folio,
			Subtotal:  s.Subtotal,
			Discount:  s.Discount,
			Total:     s.Total,
			Items:     items,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	uc.Publisher.PublishPaymentCompleted(ctx, evt)
	return &out, nil
}
