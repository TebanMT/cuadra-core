// Package repository declares the persistence contracts for the billing BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"time"

	"github.com/google/uuid"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	paymentCorrectionDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/paymentcorrection"
	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
	saleDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/sale"
	correctionDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/salecorrection"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// PaymentRepository is the persistence contract for `payments`. Append-only:
// `Update` is intentionally restricted to mutate only `notes` and
// `balance_pending` (plus the bookkeeping `version` / `updated_at`). Refunds
// and settlements are NEW rows; never UPDATE the historical row's amount.
type PaymentRepository interface {
	Create(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error)
	Update(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*paymentDomain.Payment, error)
	ListByMember(tx sharedDomain.Transaction, q ListByMemberQuery) ([]*paymentDomain.Payment, int, error)
	ListByGymBetweenDates(tx sharedDomain.Transaction, q ListByGymQuery) ([]*paymentDomain.Payment, int, error)
	// AggregateByGymBetweenDates — totales de la MISMA ventana y filtros que
	// ListByGymBetweenDates pero sobre TODAS las filas (Page/PageSize se
	// ignoran). Existe porque sumar la página visible en el use case
	// sub-reportaba el "Cobrado" del período en cuanto había más de una
	// página, y el número cambiaba al paginar.
	AggregateByGymBetweenDates(tx sharedDomain.Transaction, q ListByGymQuery) (ListByGymAggregates, error)
	// HasRefundFor reports whether a UC-022 refund row already references the
	// given parent payment. Used to enforce DA-22.1 single-refund.
	HasRefundFor(tx sharedDomain.Transaction, parentPaymentID uuid.UUID) (bool, error)
	// MaxFolioForConcept returns the most-recent folio (by lexicographic max)
	// for a given (gym, concept) — used by FolioGenerator. Empty string when
	// none exists. Implementations MUST take a row lock so concurrent
	// transactions serialise (Postgres: FOR UPDATE).
	MaxFolioForConcept(tx sharedDomain.Transaction, gymID uuid.UUID, concept string) (string, error)
	// SumPendingByMember suma balance_pending de TODOS los pagos vivos del
	// socio (cualquier concepto). Es un agregado del lado del repo porque el
	// historial se pagina: sumar en el FE sólo la página visible escondería
	// deudas viejas — mismo razonamiento que total_paid en el listado de gym.
	SumPendingByMember(tx sharedDomain.Transaction, gymID, memberID uuid.UUID) (float64, error)
}

type RefundBalance struct {
	Root       *paymentDomain.Payment
	Collected  float64
	Recognized float64
	Refunded   float64
	Refundable float64
	// Source* is scoped to the exact row requested. For a root obligation,
	// Collected also includes its later settlements; SourceRefundable does
	// not. This prevents a refund click on the original row from silently
	// returning every subsequent abono.
	SourceRefunded   float64
	SourceRefundable float64
}

// PaymentRefundLocker serializes refunds over the root obligation. It is a
// separate capability so folio/test adapters implementing PaymentRepository
// do not need refund-specific methods.
type PaymentRefundLocker interface {
	RefundBalanceForUpdate(tx sharedDomain.Transaction, gymID, rootPaymentID uuid.UUID) (RefundBalance, error)
}

type PaymentRefundReader interface {
	RefundBalance(tx sharedDomain.Transaction, gymID, rootPaymentID uuid.UUID) (RefundBalance, error)
}

type RefundRepository interface {
	Create(tx sharedDomain.Transaction, r *refundDomain.Refund) (*refundDomain.Refund, error)
	GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*refundDomain.Refund, error)
	RefundedQuantitiesBySale(tx sharedDomain.Transaction, gymID, saleID uuid.UUID) (map[uuid.UUID]int, error)
}

// PaymentCorrectionStore is intentionally separate from PaymentRepository:
// ordinary payments remain append-only; only the audited sale-correction use
// case may change physical/recognized amounts.
type PaymentCorrectionStore interface {
	UpdateForSaleCorrection(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error)
}

// PaymentAdministrativeCorrectionStore is the narrow optimistic writer used
// only by the audited owner correction flow for membership/other income.
type PaymentAdministrativeCorrectionStore interface {
	UpdateForAdministrativeCorrection(tx sharedDomain.Transaction, p *paymentDomain.Payment, expectedVersion int) (*paymentDomain.Payment, error)
}

type PaymentCorrectionRepository interface {
	Create(tx sharedDomain.Transaction, c *paymentCorrectionDomain.Correction) (*paymentCorrectionDomain.Correction, error)
	FinalizeIdempotency(tx sharedDomain.Transaction, c *paymentCorrectionDomain.Correction) error
	GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*paymentCorrectionDomain.Correction, error)
	ListByPayment(tx sharedDomain.Transaction, gymID, paymentID uuid.UUID) ([]*paymentCorrectionDomain.Correction, error)
}

// PaymentIdempotencyStore is the narrow exceptional writer for command
// metadata. FinalizeIdempotency runs in the same transaction as the payment
// and business side effects, so a committed keyed command always has a replay
// result and an aborted command has neither.
type PaymentIdempotencyStore interface {
	GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*paymentDomain.Payment, error)
	FinalizeIdempotency(tx sharedDomain.Transaction, p *paymentDomain.Payment) error
}

// SaleCorrectionStore performs the optimistic sale+line replacement and owns
// immutable correction records. PendingAmount is derived from correction
// delta minus non-economic settlements, never copied into a mutable balance.
type SaleCorrectionStore interface {
	ApplySaleCorrection(tx sharedDomain.Transaction, sale *saleDomain.Sale, items []*saleDomain.SaleItem, expectedCorrectionVersion int) error
	CreateCorrection(tx sharedDomain.Transaction, correction *correctionDomain.Correction) (*correctionDomain.Correction, error)
	FinalizeCorrectionIdempotency(tx sharedDomain.Transaction, correction *correctionDomain.Correction) error
	GetCorrectionByID(tx sharedDomain.Transaction, gymID, correctionID uuid.UUID, forUpdate bool) (*correctionDomain.Correction, error)
	GetCorrectionByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*correctionDomain.Correction, error)
	ListCorrectionsBySale(tx sharedDomain.Transaction, gymID, saleID uuid.UUID) ([]*correctionDomain.Correction, error)
	PendingAmount(tx sharedDomain.Transaction, gymID, correctionID uuid.UUID, forUpdate bool) (float64, error)
}

// ListByMemberQuery backs UC-021. ConceptFilter is one of "" (all),
// "membership", "product", "balance_settlement", "refund", "other".
type ListByMemberQuery struct {
	GymID         uuid.UUID
	MemberID      uuid.UUID
	ConceptFilter string
	From          *time.Time
	To            *time.Time
	Page          int
	PageSize      int
}

// ListByGymQuery is a generic gym-scoped listing used by reports and the
// future cash-close flow. MethodFilter ("cash"/"transfer"/"card", vacío =
// todos) filtra por método de pago — lo usa la pantalla de cobros para que
// el filtro sea del lado del server y los totales cubran el mismo set que
// la lista (filtrar en el FE sólo la página visible mentía).
type ListByGymQuery struct {
	GymID         uuid.UUID
	From          time.Time
	To            time.Time
	ConceptFilter string
	MethodFilter  string
	Page          int
	PageSize      int
}

// ListByGymAggregates — totales del set filtrado completo. NetTotal suma
// amount tal cual (los refunds viven con monto negativo, así que restan
// solos); los por-método son netos del método por la misma razón.
// RefundTotal es la magnitud devuelta (positiva) para mostrarse como
// "Devoluciones: $X" sin que el FE ande negando signos.
type ListByGymAggregates struct {
	NetTotal      float64
	RefundTotal   float64
	CashTotal     float64
	TransferTotal float64
	CardTotal     float64
}

// SaleRepository — UC-025 (RegisterSale) writes Sale + SaleItems atomically.
// Reads support receipts, refund flows, and reports/cash_close.
type SaleRepository interface {
	Create(tx sharedDomain.Transaction, s *saleDomain.Sale) (*saleDomain.Sale, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*saleDomain.Sale, error)
	GetByPaymentID(tx sharedDomain.Transaction, paymentID uuid.UUID) (*saleDomain.Sale, error)
}

// SaleItemRepository — child rows of a Sale. Carved out so reports can
// run product-level GROUP BYs without loading entire Sales.
type SaleItemRepository interface {
	CreateMany(tx sharedDomain.Transaction, items []*saleDomain.SaleItem) error
	ListBySale(tx sharedDomain.Transaction, saleID uuid.UUID) ([]*saleDomain.SaleItem, error)
	// SaleSummariesByPaymentIDs — resumen humano de los productos de cada
	// venta ("Agua 1L ×2 · Proteína"), llaveado por el payment_id de la
	// venta. Alimenta las listas de cobros: sin esto, una venta walk-in
	// (member_id NULL) se pintaba como "—". Sólo devuelve entradas para
	// payments que tienen venta viva.
	SaleSummariesByPaymentIDs(tx sharedDomain.Transaction, paymentIDs []uuid.UUID) (map[uuid.UUID]string, error)
	// SaleIDsByPaymentIDs resolves each listed payment row (root product
	// payment, settlement, or refund) to its live sale in one bulk query.
	SaleIDsByPaymentIDs(tx sharedDomain.Transaction, paymentIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
}

// CashCloseQuery backs UC-027. Returns the per-method totals + per-concept
// totals + per-operator totals for a single gym/day. The repository turns one
// SQL aggregation into the read model used by the report.
type CashCloseQuery struct {
	GymID    uuid.UUID
	Date     time.Time
	DrawerID uuid.UUID
}

// CashSessionActivityQuery scopes physical cash flow to one drawer session.
// Until writers expose drawer selection, legacy/null drawer activity belongs
// to the deterministic main drawer (drawer_id == gym_id).
type CashSessionActivityQuery struct {
	GymID           uuid.UUID
	DrawerID        uuid.UUID
	OperationalDate time.Time
	OpenedAt        time.Time
	AsOf            time.Time
}

type CashSessionActivity struct {
	PaymentsCash float64
	CashIn       float64
	CashOut      float64
	Net          float64
	Watermark    time.Time
}

type CashDrawerActivity struct {
	DrawerID       uuid.UUID
	ActivityCash   float64
	LastActivityAt time.Time
}

type CashSessionCoverageQuery struct {
	GymID uuid.UUID
	From  time.Time
	To    time.Time
}

type CashSessionCoverage struct {
	ActiveDays        int
	ActiveSessions    int
	MissingActiveDays int
	// UncoveredActivityDays includes activity in a drawer without a session
	// and activity captured after the latest close/withdrawal. It closes the
	// gap where "there is some session that day" was incorrectly considered
	// complete even though later cash had never been counted.
	UncoveredActivityDays           int
	OpenSessions                    int
	ClosedUnverifiedSessions        int
	StaleSessions                   int
	ReconciledSessions              int
	WithdrawnSessions               int
	UnknownOpeningSessions          int
	AdjustedAfterWithdrawalSessions int
	Complete                        bool
}

// CashCloseTotals is the read model produced by the repository for UC-027.
// All amounts are signed (refunds are negative). Cash rows are scoped to the
// selected physical drawer; card/transfer rows are gym-wide reference totals
// because they do not belong to a drawer. Only ByMethod["cash"] participates
// in the physical reconciliation. ByConcept carries both the total and the row
// count so the FE can render "Mensualidades · 4 cobros" without a second
// roundtrip.
type CashCloseTotals struct {
	ByMethod   map[string]float64
	ByConcept  map[string]ConceptTotal
	ByOperator []OperatorTotal
	GrandTotal float64
	// RefundTotal y RefundByMethod son NEGATIVOS (los refunds se guardan con
	// amount negativo). RefundByMethod permite descontar del cajón los
	// reembolsos pagados en efectivo (que físicamente sacan dinero) — antes
	// el corte sólo restaba gastos cash, no refunds cash, y reportaba un
	// faltante fantasma. La presentación al FE va en magnitud positiva.
	RefundTotal    float64
	RefundByMethod map[string]float64
	RefundCount    int
}

// ConceptTotal — total + count by payment concept.
type ConceptTotal struct {
	Total float64
	Count int
}

// OperatorTotal — per-operator daily summary. OperatorName is resolved at
// the repository edge (JOIN users) so the use case doesn't have to fan out.
// SalesN counts product sales (concept='product') attributed to the operator,
// independent of PaymentsN.
type OperatorTotal struct {
	OperatorID   uuid.UUID
	OperatorName string
	Total        float64
	PaymentsN    int
	SalesN       int
}

// CashCloseReader is the read seam of UC-027. Implementations live alongside
// the SaleRepository (Postgres + SQLite).
type CashLedgerEntry struct {
	ID           uuid.UUID
	RecordedAt   time.Time
	Amount       float64
	Concept      string
	Reason       string
	OperatorName string
}

type CashCloseReader interface {
	CashEntries(tx sharedDomain.Transaction, q CashCloseQuery) ([]CashLedgerEntry, error)
	Aggregate(tx sharedDomain.Transaction, q CashCloseQuery) (*CashCloseTotals, error)
	SessionActivity(tx sharedDomain.Transaction, q CashSessionActivityQuery) (*CashSessionActivity, error)
	CashDrawers(tx sharedDomain.Transaction, gymID uuid.UUID, date time.Time) ([]CashDrawerActivity, error)
	SessionCoverage(tx sharedDomain.Transaction, q CashSessionCoverageQuery) (*CashSessionCoverage, error)
	// HasCashActivityAfter detects cash-affecting operations captured after a
	// close snapshot. Card/transfer payments are intentionally ignored because
	// they do not change the physical drawer.
	HasCashActivityAfter(tx sharedDomain.Transaction, q CashCloseQuery, after time.Time) (bool, error)
}

// CashCloseEventRepository persists the cierre event (UC-027 step 4).
type CashCloseEventRepository interface {
	Create(tx sharedDomain.Transaction, e *cashCloseDomain.CashCloseEvent) (*cashCloseDomain.CashCloseEvent, error)
	Update(tx sharedDomain.Transaction, e *cashCloseDomain.CashCloseEvent) (*cashCloseDomain.CashCloseEvent, error)
	GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*cashCloseDomain.CashCloseEvent, error)
	GetByNaturalKey(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID, date time.Time, sequence int) (*cashCloseDomain.CashCloseEvent, error)
	GetByDate(tx sharedDomain.Transaction, gymID uuid.UUID, date time.Time) (*cashCloseDomain.CashCloseEvent, error)
	ListByDate(tx sharedDomain.Transaction, gymID uuid.UUID, date time.Time) ([]*cashCloseDomain.CashCloseEvent, error)
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*cashCloseDomain.CashCloseEvent, error)
	LatestBefore(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID, date time.Time) (*cashCloseDomain.CashCloseEvent, error)
	CreateTransfer(tx sharedDomain.Transaction, transfer *cashCloseDomain.CashTransfer) (*cashCloseDomain.CashTransfer, error)
}

// CashDrawerRepository owns the stable configuration catalog. Deactivating a
// drawer is an upsert (active=false), never deletion, so historical sessions
// and reports retain a human-readable name on every device.
type CashDrawerRepository interface {
	Create(tx sharedDomain.Transaction, drawer *cashCloseDomain.CashDrawer) (*cashCloseDomain.CashDrawer, error)
	Update(tx sharedDomain.Transaction, drawer *cashCloseDomain.CashDrawer, expectedVersion int) (*cashCloseDomain.CashDrawer, error)
	GetByID(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID) (*cashCloseDomain.CashDrawer, error)
	GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*cashCloseDomain.CashDrawer, error)
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, includeInactive bool) ([]*cashCloseDomain.CashDrawer, error)
	HasOpenActivity(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID) (bool, error)
}
