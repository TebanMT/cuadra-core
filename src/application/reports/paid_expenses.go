package reports

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// PaidExpenses uses the report's canonical purchase projection, including legacy
// restocks. Reading the two sources in one snapshot keeps pages and totals consistent.
type PaidExpenses struct {
	Reader Reader
	UoW    sharedDomain.UnitOfWork
	Gyms   gymRepo.GymRepository
}

func NewPaidExpenses(r Reader, u sharedDomain.UnitOfWork, g gymRepo.GymRepository) *PaidExpenses {
	return &PaidExpenses{r, u, g}
}

type PaidExpensesInput struct {
	GymID          uuid.UUID
	From, To       time.Time
	Query          string
	Page, PageSize int
}
type PaidExpenseItem struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Date        string  `json:"date"`
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
	ExpenseID   string  `json:"expense_id,omitempty"`
	MovementID  string  `json:"movement_id,omitempty"`
	ProductID   string  `json:"product_id,omitempty"`
	Quantity    int     `json:"quantity,omitempty"`
	UnitCost    float64 `json:"unit_cost,omitempty"`
}
type PaidExpensesOutput struct {
	MissingPurchaseAmountCount int               `json:"missing_purchase_amount_count"`
	Items                      []PaidExpenseItem `json:"items"`
	Total                      int               `json:"total"`
	TotalAmount                float64           `json:"total_amount"`
	Page                       int               `json:"page"`
	PageSize                   int               `json:"page_size"`
}

func (uc *PaidExpenses) Execute(ctx context.Context, in PaidExpensesInput) (*PaidExpensesOutput, error) {
	if err := validateReportWindow(PeriodCustom, &in.From, &in.To); err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	if in.Page < 1 {
		in.Page = 1
	}
	if in.PageSize < 1 {
		in.PageSize = 50
	}
	if in.PageSize > 200 {
		in.PageSize = 200
	}
	out := &PaidExpensesOutput{Items: []PaidExpenseItem{}, Page: in.Page, PageSize: in.PageSize}
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		_, tzName := localTodayAndTZ(tx, uc.Gyms, in.GymID, time.Now().UTC())
		expenses, err := uc.Reader.ListExpensesBetween(tx, in.GymID, in.From, in.To, -1)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		purchases, err := uc.Reader.ListInventoryCostsBetween(tx, in.GymID, tzName, in.From, in.To, -1)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if reader, ok := uc.Reader.(CanonicalFinancialReader); ok {
			snapshot, err := reader.CanonicalFinancialBetween(tx, in.GymID, tzName, in.From, in.To)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			out.MissingPurchaseAmountCount = snapshot.LegacyPurchaseCount
		}
		rows := make([]PaidExpenseItem, 0, len(expenses)+len(purchases))
		for _, e := range expenses {
			label := map[string]string{"renta": "Renta", "servicios": "Servicios", "nomina": "Nómina", "mantenimiento": "Mantenimiento", "marketing": "Publicidad", "insumos_no_inventariables": "Materiales", "impuestos_y_permisos": "Impuestos y permisos", "otros": "Otros"}[e.Category]
			if e.Description != nil && strings.TrimSpace(*e.Description) != "" {
				label = *e.Description
			}
			if label == "" {
				label = "Gasto"
			}
			rows = append(rows, PaidExpenseItem{ID: "expense:" + e.ID.String(), Kind: "expense", ExpenseID: e.ID.String(), Date: e.ExpenseDate.Format("2006-01-02"), Description: label, Amount: e.Amount})
		}
		for _, p := range purchases {
			rows = append(rows, PaidExpenseItem{ID: "purchase:" + p.MovementID.String(), Kind: "purchase", MovementID: p.MovementID.String(), ProductID: p.ProductID.String(), Date: p.OccurredAt.Format("2006-01-02"), Description: fmt.Sprintf("%s · %d piezas", p.ProductName, p.Delta), Amount: p.CostTotal, Quantity: p.Delta, UnitCost: p.CostUnit})
		}
		query := strings.ToLower(strings.TrimSpace(in.Query))
		filtered := rows[:0]
		var cents int64
		for _, row := range rows {
			if query != "" && !strings.Contains(strings.ToLower(row.Description), query) {
				continue
			}
			filtered = append(filtered, row)
			cents += int64(math.Round(row.Amount * 100))
		}
		sort.Slice(filtered, func(i, j int) bool {
			if filtered[i].Date != filtered[j].Date {
				return filtered[i].Date > filtered[j].Date
			}
			return filtered[i].ID < filtered[j].ID
		})
		out.Total = len(filtered)
		out.TotalAmount = float64(cents) / 100
		// Check the page before multiplying user input to avoid integer overflow.
		if in.Page > (len(filtered)+in.PageSize-1)/in.PageSize {
			return nil
		}
		start := (in.Page - 1) * in.PageSize
		end := start + in.PageSize
		if end > len(filtered) {
			end = len(filtered)
		}
		out.Items = filtered[start:end]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
