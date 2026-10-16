package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	product "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	repo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	movement "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type CountLine struct {
	ProductID uuid.UUID `json:"product_id"`
	Revision  string    `json:"revision"`
	Quantity  *int      `json:"quantity"`
}
type CountInput struct {
	ID               uuid.UUID   `json:"id"`
	Lines            []CountLine `json:"lines"`
	Reason           string      `json:"reason"`
	PurchasesChecked bool        `json:"purchases_checked"`
	GymID            uuid.UUID   `json:"-"`
	ActorID          uuid.UUID   `json:"-"`
	ActorRole        string      `json:"-"`
}
type CountRow struct {
	ProductID uuid.UUID `json:"product_id"`
	Name      string    `json:"name"`
	Stock     int       `json:"stock"`
	Revision  string    `json:"revision"`
	Available bool      `json:"available"`
}
type CountResult struct {
	ID          uuid.UUID         `json:"id"`
	ConfirmedAt time.Time         `json:"confirmed_at"`
	Items       []CountAdjustment `json:"items"`
}
type CountAdjustment struct {
	ProductID  uuid.UUID `json:"product_id"`
	Registered int       `json:"registered"`
	Counted    int       `json:"counted"`
	Delta      int       `json:"delta"`
	MovementID uuid.UUID `json:"movement_id"`
}

// CountConflict carries fresh quantities for explicit review; nothing is saved.
type CountConflict struct {
	Items []CountRow `json:"items"`
}

func (*CountConflict) Error() string {
	return "Hay productos que cambiaron. Revisa y vuelve a contar los señalados."
}

type StockCount struct {
	Products  repo.ProductRepository
	Counts    repo.StockCountRepository
	Movements repo.StockMovementRepository
	UoW       shared.UnitOfWork
	Audit     audit.Recorder
}

func NewStockCount(p repo.ProductRepository, c repo.StockCountRepository, m repo.StockMovementRepository, u shared.UnitOfWork, a audit.Recorder) *StockCount {
	return &StockCount{p, c, m, u, a}
}
func CountSnapshot(p *product.Product) CountRow {
	raw := fmt.Sprintf("%s:%d:%d:%d:%t:%v", p.ID, p.Version, p.Stock, p.UpdatedAt.UnixNano(), p.Active, p.DeletedAt)
	sum := sha256.Sum256([]byte(raw))
	return CountRow{p.ID, p.Name, p.Stock, hex.EncodeToString(sum[:]), p.Active && p.DeletedAt == nil}
}
func (uc *StockCount) Execute(ctx context.Context, in CountInput) (*CountResult, error) {
	if in.ActorRole != "owner" && in.ActorRole != "operator" {
		return nil, shared.NewBusinessError(errors.New("No tienes permiso para contar productos."), "")
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.ID == uuid.Nil || in.GymID == uuid.Nil || in.ActorID == uuid.Nil || len(in.Lines) == 0 || len(in.Lines) > 200 || len(in.Reason) < 3 || len(in.Reason) > 200 || !in.PurchasesChecked {
		return nil, shared.NewValidationError(errors.New("Revisa las compras pendientes e indica el motivo del conteo (3 a 200 caracteres). Cuenta hasta 200 productos a la vez."))
	}
	in.Lines = append([]CountLine(nil), in.Lines...)
	sort.Slice(in.Lines, func(i, j int) bool { return in.Lines[i].ProductID.String() < in.Lines[j].ProductID.String() })
	for i, line := range in.Lines {
		if line.ProductID == uuid.Nil || line.Quantity == nil || *line.Quantity < 0 || *line.Quantity > 1000000 || line.Revision == "" || (i > 0 && line.ProductID == in.Lines[i-1].ProductID) {
			return nil, shared.NewValidationError(errors.New("Escribe cantidades enteras de 0 a 1,000,000, sin repetir productos."))
		}
	}
	raw, _ := json.Marshal(in)
	sum := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(sum[:])
	key := "stock-count:" + in.ID.String()
	out := CountResult{ID: in.ID, ConfirmedAt: time.Now().UTC(), Items: []CountAdjustment{}}
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := uc.Counts.LockCount(tx, in.GymID, in.ID); err != nil {
			return err
		}
		existing, err := uc.Movements.GetByIdempotencyKey(tx, in.GymID, key)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.IdempotencyFingerprint != fingerprint {
				return shared.NewBusinessError(prodErrors.ErrAdjustmentIdempotencyConflict, "")
			}
			return json.Unmarshal(existing.IdempotencyResult, &out)
		}
		products := make([]*product.Product, len(in.Lines))
		conflicts := &CountConflict{}
		for i, line := range in.Lines {
			p, err := uc.Counts.CountProduct(tx, in.GymID, line.ProductID)
			if err != nil {
				return err
			}
			if p == nil {
				conflicts.Items = append(conflicts.Items, CountRow{ProductID: line.ProductID})
				continue
			}
			row := CountSnapshot(p)
			if !row.Available || row.Revision != line.Revision {
				conflicts.Items = append(conflicts.Items, row)
			}
			products[i] = p
		}
		if len(conflicts.Items) > 0 {
			return conflicts
		}
		for i, p := range products {
			qty := *in.Lines[i].Quantity
			before := p.Stock
			delta := qty - before
			if delta != 0 {
				if _, err := p.SetStock(qty, out.ConfirmedAt); err != nil {
					return shared.NewValidationError(err)
				}
				if _, err := uc.Products.Update(tx, p); err != nil {
					return err
				}
			}
			out.Items = append(out.Items, CountAdjustment{p.ID, before, qty, delta, uuid.NewSHA1(in.ID, []byte(p.ID.String()))})
		}
		result, _ := json.Marshal(out)
		for i, row := range out.Items {
			mv, err := movement.New(row.MovementID, in.GymID, row.ProductID, in.ActorID, movement.TypeCountCorrection, row.Delta, &in.Reason, nil, nil, out.ConfirmedAt)
			if err != nil {
				return err
			}
			mv.MarkAsCapture()
			rowKey := key
			rowResult := result
			if i > 0 {
				rowKey += ":" + row.ProductID.String()
				rowResult, _ = json.Marshal(row)
			}
			if err = mv.WithIdempotency(rowKey, fingerprint, rowResult); err != nil {
				return err
			}
			if _, err = uc.Movements.Create(tx, mv); err != nil {
				return err
			}
		}
		return uc.Audit.Record(ctx, tx, audit.Entry{GymID: in.GymID, EntityType: "stock_counts", EntityID: in.ID, Action: "stock_count_confirmed", ActorUserID: &in.ActorID, Changes: map[string]any{"reason": in.Reason, "items": out.Items, "purchases_checked": true}, At: out.ConfirmedAt})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
