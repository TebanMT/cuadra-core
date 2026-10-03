//go:build server

package sync

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	phonepkg "github.com/cuadra/cuadra-core/src/shared/phone"
)

// Projector materialises a single sync payload into the domain table that
// owns it. Runs inside the same serializable transaction as the
// sync_entities upsert (ADR-008 §3.1) so a partial application is impossible.
type Projector func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error

// financialConflictError marks a domain-level sync rejection that must not be
// retried as a transient database failure. It is deliberately distinct from a
// unique violation: the row is structurally valid, but accepting it would
// break a financial aggregate that another offline device changed first.
type financialConflictError struct {
	message string
}

func (e *financialConflictError) Error() string { return e.message }

func newFinancialConflict(format string, args ...any) error {
	return &financialConflictError{message: fmt.Sprintf(format, args...)}
}

// jsonbColumns lists the columns per table that Postgres expects as JSONB.
// The generic projector wraps these in `?::jsonb` casts because the wire
// payload carries them as JSON values (objects/arrays) that round-trip via
// re-marshal.
var jsonbColumns = map[string]map[string]bool{
	"gyms":               {"payment_methods": true, "kiosk_settings": true, "charge_settings": true},
	"notification_queue": {"payload": true},
	"audit_log":          {"changes": true},
	// payments.breakdown carries the per-concept line items as a JSON
	// array (BreakdownLine[]). Without this entry pgx serialises the
	// Go slice as a Postgres record/tuple and the upsert fails with
	// "expression is of type record" (SQLSTATE 42804).
	"payments": {"breakdown": true, "idempotency_result": true},
	"payment_corrections": {
		"before_snapshot": true, "after_snapshot": true, "idempotency_result": true,
	},
	"refunds":         {"idempotency_result": true},
	"stock_movements": {"idempotency_result": true},
	// Correction snapshots intentionally store integer cents so the audit
	// trail is exact and does not inherit JSON floating-point ambiguity.
	"sale_corrections": {"before_snapshot": true, "after_snapshot": true, "idempotency_result": true},
	// subscription_events.raw_payload — el cuerpo del webhook normalizado
	// como objeto JSON. Mismo tratamiento que cualquier otra columna JSONB.
	"subscription_events": {"raw_payload": true},
}

// byteaColumns lists BYTEA columns that arrive in the payload base64-encoded.
// The projector base64-decodes them so Postgres receives raw bytes.
var byteaColumns = map[string]map[string]bool{
	"member_fingerprints": {"template_encrypted": true},
	"gyms":                {"whatsapp_business_token_enc": true},
}

// payloadKeyAliases maps payload field names to the actual table column. The
// sidecar emits `deleted_at_ms` for member_fingerprints (epoch ms) instead of
// `deleted_at` — projector translates the alias on the way in. New aliases
// belong here, not scattered across each projector.
var payloadKeyAliases = map[string]map[string]string{
	"member_fingerprints": {"deleted_at_ms": "deleted_at"},
}

// timestamptzColumns lists the extra columns whose Postgres type is
// TIMESTAMPTZ but whose name does NOT end in `_at`, so the suffix
// heuristic in isTimestampColumn would miss them. Without an entry here,
// epoch-ms ints from the sidecar payload reach pgx as raw float64 and the
// upsert dies with "cannot find encode plan for OID 1184".
//
// Discover new entries by grepping `db_migrations/postgres/` for
// `TIMESTAMPTZ` and filtering out names ending in `_at`. Mirror to SQLite
// is irrelevant — sidecar stores epoch-ms regardless of column name.
var timestamptzColumns = map[string]map[string]bool{
	"challenges":         {"measurement_t0_deadline": true, "measurement_t1_start": true},
	"notification_queue": {"scheduled_for": true},
	// subscription_events tiene tres TIMESTAMPTZ que no terminan en `_at`-
	// del-record-estándar: occurred_at sí matchea, pero period_ends_at va
	// con sufijo distinto y recorded_at es semántica BE — los tres llegan
	// como epoch-ms del sidecar (en realidad sólo cloud → sidecar pull,
	// pero declararlos hace simétrico el wire shape si algún día cambia).
	"subscription_events": {"period_ends_at": true, "occurred_at": true, "recorded_at": true},
}

// notNullStringColumns lists columns declared NOT NULL in Postgres whose
// dominio semántico trata "" como ausencia válida (ej. users.email tras la
// migración 019: el operador PIN-only no lleva email; "" = sin correo).
// Sin esta lista, nullifyEmptyString colapsa "" → NULL y Postgres rechaza
// con `null value in column "email" violates not-null constraint` (23502).
//
// Discover new entries with:
//
//	grep -E 'TEXT NOT NULL|VARCHAR\([0-9]+\) NOT NULL' db_migrations/postgres/*.sql
//
// y verificar contra el enqueue helper de cada repo SQLite: si la columna se
// envía como `""` cuando es opcional en el dominio, debe vivir aquí.
var notNullStringColumns = map[string]map[string]bool{
	"users": {"email": true},
	// phone: NOT NULL con "" = socio SIN teléfono (alta explícita con el
	// check del FE). Nullificarlo rompería el INSERT en postgres (23502).
	"members": {"phone": true},
	// body: NOT NULL con "" como valor VÁLIDO y común — significa "usa el
	// texto default de la librería" (Standard nunca edita el body, sólo el
	// switch; enqueueTemplate siempre emite ""). Nullificarlo rompía la
	// proyección de todo toggle con 23502.
	"notification_templates": {"body": true},
}

// projectors is the dispatch table. Every entry in SyncedTables must have a
// projector registered or push fails with `missing_projector` (ADR-008 §3.1
// — fail loud, do not degrade silently). Today every projector delegates to
// the same generic implementation; the map exists so future entity-specific
// behaviour (e.g. denormalisation, side effects) can replace one entry
// without touching the rest.
var projectors = func() map[string]Projector {
	m := make(map[string]Projector, len(SyncedTables))
	var membershipsTable, membersTable, usersTable, templatesTable, cashSessionsTable EntityTable
	var paymentsTable, salesTable, saleItemsTable EntityTable
	var refundsTable, refundItemsTable, saleCorrectionsTable, paymentCorrectionsTable EntityTable
	for i := range SyncedTables {
		t := SyncedTables[i]
		if t.Type == "memberships" {
			membershipsTable = t
		}
		if t.Type == "members" {
			membersTable = t
		}
		if t.Type == "users" {
			usersTable = t
		}
		if t.Type == "notification_templates" {
			templatesTable = t
		}
		if t.Type == "cash_close_events" {
			cashSessionsTable = t
		}
		if t.Type == "payments" {
			paymentsTable = t
		}
		if t.Type == "sales" {
			salesTable = t
		}
		if t.Type == "sale_items" {
			saleItemsTable = t
		}
		if t.Type == "refunds" {
			refundsTable = t
		}
		if t.Type == "refund_items" {
			refundItemsTable = t
		}
		if t.Type == "sale_corrections" {
			saleCorrectionsTable = t
		}
		if t.Type == "payment_corrections" {
			paymentCorrectionsTable = t
		}
		m[t.Type] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
			return projectGeneric(g, t, gymID, entityID, payload)
		}
	}
	// members necesita reconciliación del número de socio al cruzar la
	// frontera de sync (ADR-010 §2.3): si la fila entrante reclama un número
	// que otro socio vivo del gym ya tiene, el que llegó después pierde — se
	// le reasigna un número nuevo y se re-encola su banner de bienvenida.
	m["members"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectMember(g, membersTable, gymID, entityID, payload)
	}
	// memberships necesita un pre-step: el índice parcial único
	// `uq_memberships_member_active` permite UNA sola fila por (gym,member)
	// con status active|pending_payment. El sidecar lo respeta con el
	// 3-step dance (services.go RenewMembershipForPayment), pero al cruzar
	// la frontera de sync cada item se proyecta en su propia transacción.
	// Si el cloud quedó con dos rows activas para el mismo socio (renewal
	// que se aplicó parcial, push fuera de orden por reintentos, o ids
	// generados por dos fuentes), la inserción del nuevo activo choca con
	// 23505. La resolución consistente con el dominio es "last write
	// wins el slot": demote el otro a 'replaced' antes de proyectar.
	m["memberships"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectMembership(g, membershipsTable, gymID, entityID, payload)
	}
	// users lleva una salvaguarda de credenciales: el CLOUD es la autoridad
	// del password del dashboard del dueño. Ver projectUser / guardUserCredential.
	m["users"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectUser(g, usersTable, gymID, entityID, payload)
	}
	// notification_templates — mismo pre-step que memberships pero sobre
	// el índice parcial `uq_notification_templates_gym_key` (UNA fila viva
	// por (gym, template_key)). Si el cloud ya tiene un override vivo de
	// esa llave con OTRO id (creado cloud-side, o carrera de dos devices
	// offline), el INSERT del entrante choca 23505 y ese item del push se
	// atora reintentando para siempre. Last write wins el slot: soft-delete
	// del otro antes de proyectar.
	m["notification_templates"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectTemplateOverride(g, templatesTable, gymID, entityID, payload)
	}
	// A cash session has a deterministic natural slot. Old releases used a
	// random close UUID, so an offline device that missed that history may push
	// the same slot with the new deterministic UUID. Tombstone the legacy slot
	// atomically and project the deterministic row; future pulls converge.
	m["products"] = projectProductWithReceipts
	m["cash_close_events"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectCashSession(g, cashSessionsTable, gymID, entityID, payload)
	}
	// A refund Payment is the ledger half of a Refund aggregate and reaches
	// the queue first. Guard it too: rejecting only the later refunds row would
	// leave an orphan negative ledger entry that still changes reports.
	m["payments"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectPayment(g, paymentsTable, gymID, entityID, payload)
	}
	// A Sale is the mutable half of the correction aggregate. Once an
	// immutable correction has claimed a revision, a late LWW payload from a
	// different desk may not rewrite that revision to incompatible totals.
	m["sales"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectSale(g, salesTable, gymID, entityID, payload)
	}
	m["sale_items"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectSaleItem(g, saleItemsTable, gymID, entityID, payload)
	}
	// Refunds and sale corrections are aggregate operations. Row-level LWW is
	// insufficient when two desks work offline: both rows have different IDs,
	// so both would otherwise be inserted. These projectors lock the aggregate
	// root and re-check the same monetary/version invariants as the command
	// layer before materialising the second row.
	m["refunds"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectRefund(g, refundsTable, gymID, entityID, payload)
	}
	m["refund_items"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectRefundItem(g, refundItemsTable, gymID, entityID, payload)
	}
	m["sale_corrections"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectSaleCorrection(g, saleCorrectionsTable, gymID, entityID, payload)
	}
	m["payment_corrections"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectPaymentCorrection(g, paymentCorrectionsTable, gymID, entityID, payload)
	}
	return m
}()

func projectSale(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	raw, err := decodeProjectorPayload("sales", payload)
	if err != nil {
		return err
	}
	incomingDeleted := raw["deleted_at"] != nil
	correctionVersion := 0
	if value, present := raw["correction_version"]; present && value != nil {
		var ok bool
		correctionVersion, ok = numericInt(value)
		if !ok {
			return newFinancialConflict("venta %s: versión de corrección inválida", entityID)
		}
	}
	if correctionVersion < 0 {
		return newFinancialConflict("venta %s: versión de corrección inválida", entityID)
	}
	incomingPaymentID, err := projectorUUID(raw, "payment_id")
	if err != nil {
		return err
	}

	// Lock the mutable aggregate root even when there is no accepted correction
	// yet. This serialises the last Sale payload with the immutable correction
	// projector, closing the window where a losing desk could overwrite the
	// winner immediately after its audit row committed.
	var existingLink struct {
		PaymentID uuid.UUID `gorm:"column:payment_id"`
	}
	linkResult := g.Raw(`SELECT payment_id FROM sales WHERE gym_id=? AND id=?`,
		gymID, entityID).Scan(&existingLink)
	if linkResult.Error != nil {
		return fmt.Errorf("projector sales: read payment link: %w", linkResult.Error)
	}
	if linkResult.RowsAffected > 0 && existingLink.PaymentID != incomingPaymentID {
		return newFinancialConflict("venta %s: el cobro vinculado no se puede reemplazar", entityID)
	}
	var lockedPayment struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	paymentLock := g.Raw(`SELECT id FROM payments WHERE gym_id=? AND id=? FOR UPDATE`,
		gymID, incomingPaymentID).Scan(&lockedPayment)
	if paymentLock.Error != nil {
		return fmt.Errorf("projector sales: lock payment: %w", paymentLock.Error)
	}
	var current struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	locked := g.Raw(`SELECT id FROM sales WHERE gym_id=? AND id=? FOR UPDATE`,
		gymID, entityID).Scan(&current)
	if locked.Error != nil {
		return fmt.Errorf("projector sales: lock corrected sale: %w", locked.Error)
	}

	var latest struct {
		Expected       int             `gorm:"column:expected_sale_version"`
		CorrectionType string          `gorm:"column:correction_type"`
		After          json.RawMessage `gorm:"column:after_snapshot"`
	}
	latestResult := g.Raw(`SELECT expected_sale_version,correction_type,after_snapshot
		FROM sale_corrections WHERE gym_id=? AND sale_id=? AND deleted_at IS NULL
		ORDER BY expected_sale_version DESC LIMIT 1 FOR UPDATE`, gymID, entityID).Scan(&latest)
	if latestResult.Error != nil {
		return fmt.Errorf("projector sales: read accepted correction: %w", latestResult.Error)
	}
	if latestResult.RowsAffected > 0 {
		acceptedVersion := latest.Expected + 1
		if correctionVersion < acceptedVersion {
			return newFinancialConflict(
				"venta rechazada: la nube ya confirmó la corrección %d y el escritorio envió la versión %d",
				acceptedVersion, correctionVersion,
			)
		}
		if correctionVersion == acceptedVersion {
			wantDeleted := latest.CorrectionType == "annul"
			if incomingDeleted != wantDeleted {
				return newFinancialConflict(
					"venta rechazada: su estado activo/anulado no coincide con la corrección %d confirmada", acceptedVersion,
				)
			}
			after, parseErr := projectorSnapshot(latest.After)
			if parseErr != nil {
				return fmt.Errorf("projector sales: decode accepted snapshot: %w", parseErr)
			}
			wantSubtotal, subtotalOK := projectorIntegralInt64(after["subtotal_cents"])
			wantDiscount, discountOK := projectorIntegralInt64(after["discount_cents"])
			wantTotal, totalOK := projectorIntegralInt64(after["total_cents"])
			gotSubtotal, subtotalErr := projectorMoneyCents(raw, "subtotal")
			gotDiscount, discountErr := projectorMoneyCents(raw, "discount")
			gotTotal, totalErr := projectorMoneyCents(raw, "total")
			if !subtotalOK || !discountOK || !totalOK || subtotalErr != nil || discountErr != nil || totalErr != nil ||
				wantSubtotal != gotSubtotal || wantDiscount != gotDiscount || wantTotal != gotTotal {
				return newFinancialConflict(
					"venta rechazada: sus totales no coinciden con la corrección %d ya confirmada", acceptedVersion,
				)
			}
		}
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

type projectorSaleLine struct {
	SaleItemID     uuid.UUID
	ProductID      uuid.UUID
	ProductName    string
	UnitPriceCents int64
	UnitCostCents  *int64
	Quantity       int
	LineTotalCents int64
}

func projectSaleItem(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	raw, err := decodeProjectorPayload("sale_items", payload)
	if err != nil {
		return err
	}
	saleID, err := projectorUUID(raw, "sale_id")
	if err != nil {
		return err
	}
	var link struct {
		PaymentID uuid.UUID `gorm:"column:payment_id"`
	}
	linkResult := g.Raw(`SELECT payment_id FROM sales WHERE gym_id=? AND id=?`,
		gymID, saleID).Scan(&link)
	if linkResult.Error != nil {
		return fmt.Errorf("projector sale_items: read sale link: %w", linkResult.Error)
	}
	if linkResult.RowsAffected == 0 {
		return projectGeneric(g, table, gymID, entityID, payload)
	}
	var lockedPayment struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	if result := g.Raw(`SELECT id FROM payments WHERE gym_id=? AND id=? FOR UPDATE`,
		gymID, link.PaymentID).Scan(&lockedPayment); result.Error != nil {
		return fmt.Errorf("projector sale_items: lock payment: %w", result.Error)
	}
	var sale struct {
		CorrectionVersion int `gorm:"column:correction_version"`
	}
	saleResult := g.Raw(`SELECT correction_version FROM sales
		WHERE gym_id=? AND id=? AND payment_id=? FOR UPDATE`,
		gymID, saleID, link.PaymentID).Scan(&sale)
	if saleResult.Error != nil {
		return fmt.Errorf("projector sale_items: lock sale: %w", saleResult.Error)
	}
	if saleResult.RowsAffected == 0 {
		return newFinancialConflict("la venta cambió mientras se sincronizaba uno de sus productos")
	}
	var latest struct {
		Expected int             `gorm:"column:expected_sale_version"`
		After    json.RawMessage `gorm:"column:after_snapshot"`
	}
	latestResult := g.Raw(`SELECT expected_sale_version,after_snapshot FROM sale_corrections
		WHERE gym_id=? AND sale_id=? AND deleted_at IS NULL
		ORDER BY expected_sale_version DESC LIMIT 1 FOR UPDATE`, gymID, saleID).Scan(&latest)
	if latestResult.Error != nil {
		return fmt.Errorf("projector sale_items: read accepted correction: %w", latestResult.Error)
	}
	if latestResult.RowsAffected == 0 || sale.CorrectionVersion != latest.Expected+1 {
		return projectGeneric(g, table, gymID, entityID, payload)
	}
	after, parseErr := projectorSnapshot(latest.After)
	if parseErr != nil {
		return fmt.Errorf("projector sale_items: decode accepted correction: %w", parseErr)
	}
	lines, present, linesErr := projectorSaleLines(after["lines"])
	if linesErr != nil {
		return newFinancialConflict("la corrección aceptada contiene un detalle de productos inválido")
	}
	if !present {
		return projectGeneric(g, table, gymID, entityID, payload)
	}
	want, belongs := lines[entityID]
	if raw["deleted_at"] != nil {
		if belongs {
			return newFinancialConflict("producto de venta rechazado: la corrección confirmada todavía conserva esta línea")
		}
		return projectGeneric(g, table, gymID, entityID, payload)
	}
	if !belongs || !projectorSaleLineMatchesPayload(want, raw) {
		return newFinancialConflict(
			"producto de venta rechazado: no coincide con la corrección %d ya confirmada", sale.CorrectionVersion,
		)
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

// projectPayment keeps the append-only refund ledger within both the selected
// collection and the root obligation. Every refund of a root or one of its
// settlements serialises on the same root row, so distinct IDs from two
// offline desks cannot both pass a stale local refundable check.
func projectPayment(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	raw, err := decodeProjectorPayload("payments", payload)
	if err != nil {
		return err
	}
	concept, _ := raw["concept"].(string)
	if destination, present := raw["cash_destination"]; present && (destination != "cash_drawer" && destination != "gym_fund" || destination == "gym_fund" && concept != "other") {
		return newFinancialConflict("el destino del efectivo no corresponde al cobro")
	}

	var identity struct {
		Concept  string     `gorm:"column:concept"`
		ParentID *uuid.UUID `gorm:"column:parent_id"`
	}
	identityResult := g.Raw(`SELECT concept,parent_payment_id AS parent_id
		FROM payments WHERE gym_id=? AND id=?`, gymID, entityID).Scan(&identity)
	if identityResult.Error != nil {
		return fmt.Errorf("projector payments: read immutable identity: %w", identityResult.Error)
	}
	if identityResult.RowsAffected > 0 {
		incomingParent := projectorOptionalUUID(raw["parent_payment_id"])
		if identity.Concept != concept || !sameProjectorOptionalUUID(identity.ParentID, incomingParent) {
			return newFinancialConflict("cobro %s: su concepto y obligación vinculada no se pueden reemplazar", entityID)
		}
	}
	if concept == "product" {
		amount, amountErr := projectorMoneyCents(raw, "amount")
		balance, balanceErr := projectorMoneyCents(raw, "balance_pending")
		if amountErr != nil || balanceErr != nil || (amount == 0 && balance > 0 && projectorOptionalUUID(raw["member_id"]) == nil) {
			return newFinancialConflict("venta fiada inválida: el saldo pendiente requiere un socio")
		}
		return projectCorrectedSalePayment(g, table, gymID, entityID, raw, payload)
	}
	if concept == "membership" || concept == "other" {
		return projectAdministrativelyCorrectedPayment(g, table, gymID, entityID, raw, payload)
	}
	if concept != "refund" {
		return projectGeneric(g, table, gymID, entityID, payload)
	}
	if raw["deleted_at"] != nil {
		return newFinancialConflict("un movimiento de devolución sincronizado no se puede eliminar")
	}
	amountCents, err := projectorSignedMoneyCents(raw, "amount")
	if err != nil || amountCents >= 0 {
		return newFinancialConflict("movimiento de devolución %s: monto inválido", entityID)
	}
	refundCents := -amountCents
	sourceID, err := projectorUUID(raw, "parent_payment_id")
	if err != nil {
		return err
	}

	// Ledger rows are immutable. In particular, a higher LWW version may not
	// redirect an already accepted negative amount to another collection.
	var existing struct {
		ParentID    *uuid.UUID `gorm:"column:parent_id"`
		AmountCents int64      `gorm:"column:amount_cents"`
		Method      string     `gorm:"column:payment_method"`
	}
	existingResult := g.Raw(`SELECT parent_payment_id AS parent_id,
		ROUND(amount*100)::bigint AS amount_cents,payment_method
		FROM payments WHERE gym_id=? AND id=? AND concept='refund'
		  AND deleted_at IS NULL FOR UPDATE`, gymID, entityID).Scan(&existing)
	if existingResult.Error != nil {
		return fmt.Errorf("projector payments: lock existing refund ledger row: %w", existingResult.Error)
	}
	if existingResult.RowsAffected > 0 {
		method, _ := raw["payment_method"].(string)
		if existing.ParentID == nil || *existing.ParentID != sourceID ||
			existing.AmountCents != amountCents || existing.Method != method {
			return newFinancialConflict("el movimiento de devolución %s ya existe con otro efecto financiero", entityID)
		}
		return projectGeneric(g, table, gymID, entityID, payload)
	}
	if !atomicRefundGraphActive(g) {
		return newFinancialConflict(
			"devolución incompleta: el movimiento monetario debe sincronizarse junto con su devolución y productos; actualiza Tinta y reintenta",
		)
	}

	var source struct {
		Concept      string     `gorm:"column:concept"`
		ParentRootID *uuid.UUID `gorm:"column:parent_root_id"`
		AmountCents  int64      `gorm:"column:amount_cents"`
	}
	sourceResult := g.Raw(`SELECT concept,parent_payment_id AS parent_root_id,
		ROUND(amount*100)::bigint AS amount_cents
		FROM payments WHERE gym_id=? AND id=? AND amount>0 AND deleted_at IS NULL`,
		gymID, sourceID).Scan(&source)
	if sourceResult.Error != nil {
		return fmt.Errorf("projector payments: read refund source: %w", sourceResult.Error)
	}
	if sourceResult.RowsAffected == 0 {
		return newFinancialConflict("el cobro seleccionado para la devolución %s no existe en la nube", entityID)
	}
	rootID := sourceID
	if source.Concept == "balance_settlement" {
		if source.ParentRootID == nil {
			return newFinancialConflict("el abono seleccionado no tiene una obligación original")
		}
		rootID = *source.ParentRootID
	} else if source.Concept != "membership" && source.Concept != "product" && source.Concept != "other" {
		return newFinancialConflict("el movimiento seleccionado no admite devoluciones")
	}

	var root struct {
		AmountCents int64  `gorm:"column:amount_cents"`
		Concept     string `gorm:"column:concept"`
	}
	rootResult := g.Raw(`SELECT ROUND(amount*100)::bigint AS amount_cents,concept
		FROM payments WHERE gym_id=? AND id=? AND concept IN ('membership','product','other')
		  AND deleted_at IS NULL FOR UPDATE`, gymID, rootID).Scan(&root)
	if rootResult.Error != nil {
		return fmt.Errorf("projector payments: lock refund root: %w", rootResult.Error)
	}
	if rootResult.RowsAffected == 0 {
		return newFinancialConflict("la obligación original de la devolución %s no existe en la nube", entityID)
	}

	var sums struct {
		SettlementsCents int64 `gorm:"column:settlements_cents"`
		RefundedCents    int64 `gorm:"column:refunded_cents"`
		SourceRefunded   int64 `gorm:"column:source_refunded_cents"`
	}
	if err := g.Raw(`SELECT
		COALESCE(SUM(ROUND(p.amount*100)::bigint) FILTER (
		  WHERE p.concept='balance_settlement' AND p.parent_payment_id=?),0)::bigint AS settlements_cents,
		COALESCE(SUM(ROUND(ABS(p.amount)*100)::bigint) FILTER (
		  WHERE p.concept='refund' AND p.id<>? AND (
		    p.parent_payment_id=? OR EXISTS (
		      SELECT 1 FROM payments selected
		      WHERE selected.gym_id=p.gym_id AND selected.id=p.parent_payment_id
		        AND selected.concept='balance_settlement' AND selected.parent_payment_id=?
		        AND selected.deleted_at IS NULL))),0)::bigint AS refunded_cents,
		COALESCE(SUM(ROUND(ABS(p.amount)*100)::bigint) FILTER (
		  WHERE p.concept='refund' AND p.id<>? AND p.parent_payment_id=?),0)::bigint AS source_refunded_cents
		FROM payments p WHERE p.gym_id=? AND p.deleted_at IS NULL`,
		rootID, entityID, rootID, rootID, entityID, sourceID, gymID).Scan(&sums).Error; err != nil {
		return fmt.Errorf("projector payments: calculate refund ledger capacity: %w", err)
	}
	remaining := root.AmountCents + sums.SettlementsCents - sums.RefundedCents
	if refundCents > remaining {
		return newFinancialConflict(
			"devolución rechazada: quedan %s cobrados sin devolver y el escritorio intentó devolver %s",
			formatProjectorCents(maxInt64(remaining, 0)), formatProjectorCents(refundCents),
		)
	}
	if refundCents > source.AmountCents-sums.SourceRefunded {
		return newFinancialConflict("devolución rechazada: el cobro seleccionado ya no tiene saldo suficiente")
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

func projectCorrectedSalePayment(
	g *gorm.DB,
	table EntityTable,
	gymID, entityID uuid.UUID,
	raw map[string]any,
	payload []byte,
) error {
	var lockedPayment struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	paymentLock := g.Raw(`SELECT id FROM payments WHERE gym_id=? AND id=? FOR UPDATE`,
		gymID, entityID).Scan(&lockedPayment)
	if paymentLock.Error != nil {
		return fmt.Errorf("projector payments: lock product payment: %w", paymentLock.Error)
	}
	if paymentLock.RowsAffected == 0 {
		return projectGeneric(g, table, gymID, entityID, payload)
	}

	var sale struct {
		ID                uuid.UUID `gorm:"column:id"`
		CorrectionVersion int       `gorm:"column:correction_version"`
	}
	saleResult := g.Raw(`SELECT id,correction_version FROM sales
		WHERE gym_id=? AND payment_id=? FOR UPDATE`, gymID, entityID).Scan(&sale)
	if saleResult.Error != nil {
		return fmt.Errorf("projector payments: lock product sale: %w", saleResult.Error)
	}
	if saleResult.RowsAffected == 0 {
		return projectGeneric(g, table, gymID, entityID, payload)
	}
	var latest struct {
		Expected int             `gorm:"column:expected_sale_version"`
		After    json.RawMessage `gorm:"column:after_snapshot"`
	}
	latestResult := g.Raw(`SELECT expected_sale_version,after_snapshot
		FROM sale_corrections WHERE gym_id=? AND sale_id=? AND deleted_at IS NULL
		ORDER BY expected_sale_version DESC LIMIT 1 FOR UPDATE`, gymID, sale.ID).Scan(&latest)
	if latestResult.Error != nil {
		return fmt.Errorf("projector payments: read accepted sale correction: %w", latestResult.Error)
	}
	if latestResult.RowsAffected > 0 && sale.CorrectionVersion == latest.Expected+1 {
		after, parseErr := projectorSnapshot(latest.After)
		if parseErr != nil {
			return fmt.Errorf("projector payments: decode accepted sale correction: %w", parseErr)
		}
		if wantDeleted, present := after["payment_tombstoned"].(bool); present && (raw["deleted_at"] != nil) != wantDeleted {
			return newFinancialConflict(
				"cobro rechazado: su estado activo/anulado no coincide con la corrección %d ya confirmada", sale.CorrectionVersion,
			)
		}
		wantAmount, hasAmount := projectorIntegralInt64(after["payment_amount_cents"])
		wantRecognized, hasRecognized := projectorIntegralInt64(after["recognized_amount_cents"])
		gotAmount, amountErr := projectorMoneyCents(raw, "amount")
		gotRecognized, recognizedErr := projectorMoneyCents(raw, "recognized_amount")
		if (hasAmount && (amountErr != nil || gotAmount != wantAmount)) ||
			(hasRecognized && (recognizedErr != nil || gotRecognized != wantRecognized)) {
			return newFinancialConflict(
				"cobro rechazado: sus montos no coinciden con la corrección %d ya confirmada", sale.CorrectionVersion,
			)
		}
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

// Administrative corrections rewrite the mutable Payment first and append an
// immutable payment_corrections row afterwards. Once that row claims a payment
// version, a late LWW payload for the same version must reproduce its exact
// after snapshot; otherwise two offline desks could leave the audit trail and
// the canonical payment describing different money.
func projectAdministrativelyCorrectedPayment(
	g *gorm.DB,
	table EntityTable,
	gymID, entityID uuid.UUID,
	raw map[string]any,
	payload []byte,
) error {
	var locked struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	lockResult := g.Raw(`SELECT id FROM payments WHERE gym_id=? AND id=? FOR UPDATE`,
		gymID, entityID).Scan(&locked)
	if lockResult.Error != nil {
		return fmt.Errorf("projector payments: lock administratively corrected payment: %w", lockResult.Error)
	}
	if lockResult.RowsAffected == 0 {
		return projectGeneric(g, table, gymID, entityID, payload)
	}

	var latest struct {
		Expected int             `gorm:"column:expected_payment_version"`
		After    json.RawMessage `gorm:"column:after_snapshot"`
	}
	latestResult := g.Raw(`SELECT expected_payment_version,after_snapshot
		FROM payment_corrections WHERE gym_id=? AND payment_id=? AND deleted_at IS NULL
		ORDER BY expected_payment_version DESC LIMIT 1 FOR UPDATE`, gymID, entityID).Scan(&latest)
	if latestResult.Error != nil {
		return fmt.Errorf("projector payments: read accepted administrative correction: %w", latestResult.Error)
	}
	if latestResult.RowsAffected == 0 {
		return projectGeneric(g, table, gymID, entityID, payload)
	}
	incomingVersion, ok := numericInt(raw["version"])
	if !ok || incomingVersion <= 0 {
		return newFinancialConflict("cobro %s: versión inválida", entityID)
	}
	after, parseErr := projectorSnapshot(latest.After)
	if parseErr != nil {
		return fmt.Errorf("projector payments: decode accepted administrative correction: %w", parseErr)
	}
	want, snapshotErr := projectorAdministrativePaymentSnapshot(after)
	if snapshotErr != nil {
		return fmt.Errorf("projector payments: invalid accepted administrative correction: %w", snapshotErr)
	}
	acceptedVersion := latest.Expected + 1
	if incomingVersion < acceptedVersion {
		return newFinancialConflict(
			"cobro rechazado: la nube ya confirmó la corrección de versión %d y el escritorio envió la versión %d",
			acceptedVersion, incomingVersion,
		)
	}
	if want.Annulled {
		got, payloadErr := projectorAdministrativePaymentSnapshot(raw)
		got.Annulled = raw["deleted_at"] != nil
		if payloadErr != nil || !got.Annulled || !sameProjectorAdministrativePaymentFacts(want, got) {
			return newFinancialConflict("cobro rechazado: un ingreso extraordinario anulado no se puede reactivar ni reescribir")
		}
	}
	if incomingVersion == acceptedVersion {
		got, payloadErr := projectorAdministrativePaymentSnapshot(raw)
		got.Annulled = raw["deleted_at"] != nil
		if snapshotErr != nil || payloadErr != nil || !sameProjectorAdministrativePaymentSnapshot(want, got) {
			return newFinancialConflict(
				"cobro rechazado: no coincide con la corrección de versión %d ya confirmada", acceptedVersion,
			)
		}
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

type projectorAdminPaymentSnapshot struct {
	CashDestination string
	Version         int
	AmountCents     int64
	RecognizedCents int64
	PendingCents    int64
	Method          string
	CashDrawerID    *uuid.UUID
	PaymentDate     string
	Annulled        bool
}

func projectorAdministrativePaymentSnapshot(raw map[string]any) (projectorAdminPaymentSnapshot, error) {
	version, versionOK := numericInt(raw["version"])
	amount, amountErr := projectorMoneyCents(raw, "amount")
	recognized, recognizedErr := projectorMoneyCents(raw, "recognized_amount")
	pending, pendingErr := projectorMoneyCents(raw, "balance_pending")
	method, methodOK := raw["payment_method"].(string)
	date, dateOK := raw["payment_date"].(string)
	if !versionOK || version <= 0 || amountErr != nil || amount <= 0 || recognizedErr != nil ||
		recognized != amount || pendingErr != nil || !methodOK || !dateOK {
		return projectorAdminPaymentSnapshot{}, newFinancialConflict("snapshot de corrección de cobro inválido")
	}
	if method != "cash" && method != "transfer" && method != "card" {
		return projectorAdminPaymentSnapshot{}, newFinancialConflict("snapshot de corrección de cobro inválido")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return projectorAdminPaymentSnapshot{}, newFinancialConflict("snapshot de corrección de cobro inválido")
	}
	destination := "cash_drawer"
	if raw["cash_destination"] != nil {
		d, ok := raw["cash_destination"].(string)
		if !ok || (d != "cash_drawer" && d != "gym_fund") {
			return projectorAdminPaymentSnapshot{}, newFinancialConflict("destino del efectivo inválido")
		}
		destination = d
	}

	var drawer *uuid.UUID
	if raw["cash_drawer_id"] != nil {
		value, ok := raw["cash_drawer_id"].(string)
		parsed, err := uuid.Parse(value)
		if !ok || err != nil || parsed == uuid.Nil {
			return projectorAdminPaymentSnapshot{}, newFinancialConflict("snapshot de corrección de cobro inválido")
		}
		drawer = &parsed
	}
	annulled := false
	if value, present := raw["annulled"]; present {
		var valid bool
		annulled, valid = value.(bool)
		if !valid {
			return projectorAdminPaymentSnapshot{}, newFinancialConflict("snapshot de corrección de cobro inválido")
		}
	}
	return projectorAdminPaymentSnapshot{CashDestination: destination, Version: version, AmountCents: amount, RecognizedCents: recognized,
		PendingCents: pending, Method: method, CashDrawerID: drawer, PaymentDate: date, Annulled: annulled}, nil
}

func sameProjectorAdministrativePaymentSnapshot(a, b projectorAdminPaymentSnapshot) bool {
	return a.Version == b.Version && a.AmountCents == b.AmountCents &&
		a.RecognizedCents == b.RecognizedCents && a.PendingCents == b.PendingCents &&
		a.CashDestination == b.CashDestination && a.Method == b.Method && sameProjectorOptionalUUID(a.CashDrawerID, b.CashDrawerID) &&
		a.PaymentDate == b.PaymentDate && a.Annulled == b.Annulled
}

func sameProjectorAdministrativePaymentFacts(a, b projectorAdminPaymentSnapshot) bool {
	return a.AmountCents == b.AmountCents && a.RecognizedCents == b.RecognizedCents &&
		a.PendingCents == b.PendingCents && a.CashDestination == b.CashDestination && a.Method == b.Method &&
		sameProjectorOptionalUUID(a.CashDrawerID, b.CashDrawerID) && a.PaymentDate == b.PaymentDate
}

type lockedRefundRoot struct {
	AmountCents int64 `gorm:"column:amount_cents"`
}

// projectRefund serialises every refund aggregate on its root payment. The
// negative Payment row is only traceability; accepted Refund rows are the
// authoritative claims used here so an orphaned/retried payment cannot consume
// the limit twice.
func projectRefund(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	raw, err := decodeProjectorPayload("refunds", payload)
	if err != nil {
		return err
	}
	if raw["deleted_at"] != nil {
		return newFinancialConflict("una devolución sincronizada no se puede eliminar; requiere una corrección auditable")
	}
	rootID, err := projectorUUID(raw, "root_payment_id")
	if err != nil {
		return err
	}
	amountCents, err := projectorMoneyCents(raw, "amount")
	if err != nil || amountCents < 0 {
		return newFinancialConflict("devolución %s: monto inválido", entityID)
	}
	cancelledCents, err := projectorMoneyCents(raw, "balance_cancelled")
	if err != nil || cancelledCents < 0 {
		return newFinancialConflict("devolución %s: cancelación de saldo inválida", entityID)
	}
	if amountCents+cancelledCents <= 0 {
		return newFinancialConflict("devolución %s: no tiene efecto financiero", entityID)
	}
	kind, _ := raw["kind"].(string)
	if kind == "" {
		kind = "revenue_refund"
	}
	if kind != "revenue_refund" && kind != "overcollection_settlement" {
		return newFinancialConflict("devolución %s: tipo inválido", entityID)
	}

	// Financial rows are immutable. A newer row version may legitimately be a
	// replay from an old client, but it may not rewrite the aggregate effect.
	var existing struct {
		RootPaymentID  uuid.UUID  `gorm:"column:root_payment_id"`
		RefundPayment  *uuid.UUID `gorm:"column:refund_payment_id"`
		SaleID         *uuid.UUID `gorm:"column:sale_id"`
		CorrectionID   *uuid.UUID `gorm:"column:correction_id"`
		AmountCents    int64      `gorm:"column:amount_cents"`
		CancelledCents int64      `gorm:"column:cancelled_cents"`
		Method         string     `gorm:"column:method"`
		Kind           string     `gorm:"column:kind"`
		IdempotencyKey string     `gorm:"column:idempotency_key"`
		Fingerprint    string     `gorm:"column:idempotency_fingerprint"`
	}
	existingResult := g.Raw(`SELECT root_payment_id,
		ROUND(amount*100)::bigint AS amount_cents,
		ROUND(balance_cancelled*100)::bigint AS cancelled_cents,
		refund_payment_id,sale_id,correction_id,COALESCE(method,'') AS method,kind,
		idempotency_key,idempotency_fingerprint
		FROM refunds WHERE gym_id=? AND id=? AND deleted_at IS NULL FOR UPDATE`, gymID, entityID).Scan(&existing)
	if existingResult.Error != nil {
		return fmt.Errorf("projector refunds: lock existing row: %w", existingResult.Error)
	}
	if existingResult.RowsAffected > 0 {
		key, _ := raw["idempotency_key"].(string)
		fingerprint, _ := raw["idempotency_fingerprint"].(string)
		method, _ := raw["method"].(string)
		refundPayment := projectorOptionalUUID(raw["refund_payment_id"])
		sale := projectorOptionalUUID(raw["sale_id"])
		correction := projectorOptionalUUID(raw["correction_id"])
		if existing.RootPaymentID != rootID || existing.AmountCents != amountCents ||
			existing.CancelledCents != cancelledCents || existing.IdempotencyKey != key ||
			existing.Fingerprint != fingerprint || existing.Method != method || existing.Kind != kind ||
			!sameProjectorOptionalUUID(existing.RefundPayment, refundPayment) ||
			!sameProjectorOptionalUUID(existing.SaleID, sale) ||
			!sameProjectorOptionalUUID(existing.CorrectionID, correction) {
			return newFinancialConflict("la devolución %s ya existe con otro efecto financiero", entityID)
		}
		return projectGeneric(g, table, gymID, entityID, payload)
	}

	var root lockedRefundRoot
	rootResult := g.Raw(`SELECT ROUND(amount*100)::bigint AS amount_cents
		FROM payments WHERE gym_id=? AND id=? AND concept IN ('membership','product','other')
		  AND deleted_at IS NULL FOR UPDATE`, gymID, rootID).Scan(&root)
	if rootResult.Error != nil {
		return fmt.Errorf("projector refunds: lock root payment: %w", rootResult.Error)
	}
	if rootResult.RowsAffected == 0 {
		return newFinancialConflict("la obligación original de la devolución %s no existe en la nube", entityID)
	}

	var sums struct {
		SettlementsCents int64 `gorm:"column:settlements_cents"`
		RefundedCents    int64 `gorm:"column:refunded_cents"`
	}
	if err := g.Raw(`SELECT
		COALESCE((SELECT SUM(ROUND(p.amount*100)::bigint) FROM payments p
		  WHERE p.gym_id=? AND p.parent_payment_id=? AND p.concept='balance_settlement'
		    AND p.deleted_at IS NULL),0)::bigint AS settlements_cents,
		COALESCE((SELECT SUM(ROUND(r.amount*100)::bigint) FROM refunds r
		  WHERE r.gym_id=? AND r.root_payment_id=? AND r.id<>? AND r.deleted_at IS NULL),0)::bigint AS refunded_cents`,
		gymID, rootID, gymID, rootID, entityID).Scan(&sums).Error; err != nil {
		return fmt.Errorf("projector refunds: calculate aggregate balance: %w", err)
	}

	availableMonetary := root.AmountCents + sums.SettlementsCents - sums.RefundedCents
	if availableMonetary < 0 || amountCents > availableMonetary {
		return newFinancialConflict(
			"devolución rechazada: quedan %s cobrados sin devolver y el escritorio intentó devolver %s",
			formatProjectorCents(maxInt64(availableMonetary, 0)), formatProjectorCents(amountCents),
		)
	}

	// Debt cancellation exists only for an itemised product refund. Its stable
	// cap is the current sale obligation; it deliberately ignores the mutable
	// root.balance_pending row, which may already be coalesced to the final
	// state after several legitimate offline refunds. Overcollection
	// settlements are validated separately against their correction delta and
	// therefore do not consume the product-return cap.
	var saleID uuid.UUID
	var reconciledPendingCents *int64
	if value, ok := raw["sale_id"].(string); ok && value != "" {
		saleID, _ = uuid.Parse(value)
	}
	if kind == "revenue_refund" && (saleID != uuid.Nil || cancelledCents > 0) {
		if saleID == uuid.Nil {
			return newFinancialConflict("una cancelación de saldo debe identificar la venta")
		}
		var sale struct {
			PaymentID  uuid.UUID `gorm:"column:payment_id"`
			TotalCents int64     `gorm:"column:total_cents"`
		}
		saleResult := g.Raw(`SELECT payment_id,ROUND(total*100)::bigint AS total_cents
			FROM sales WHERE gym_id=? AND id=? FOR UPDATE`, gymID, saleID).Scan(&sale)
		if saleResult.Error != nil {
			return fmt.Errorf("projector refunds: lock sale obligation: %w", saleResult.Error)
		}
		if saleResult.RowsAffected == 0 || sale.PaymentID != rootID {
			return newFinancialConflict("la venta indicada no pertenece a la obligación devuelta")
		}
		var accepted struct {
			EconomicCents  int64 `gorm:"column:economic_cents"`
			CancelledCents int64 `gorm:"column:cancelled_cents"`
		}
		if err := g.Raw(`SELECT
			COALESCE(SUM(ROUND((amount+balance_cancelled)*100)::bigint),0)::bigint AS economic_cents,
			COALESCE(SUM(ROUND(balance_cancelled*100)::bigint),0)::bigint AS cancelled_cents
			FROM refunds WHERE gym_id=? AND root_payment_id=? AND sale_id=? AND id<>?
			  AND kind='revenue_refund' AND deleted_at IS NULL`, gymID, rootID, saleID, entityID).
			Scan(&accepted).Error; err != nil {
			return fmt.Errorf("projector refunds: calculate sale refund capacity: %w", err)
		}
		incomingEconomic := amountCents + cancelledCents
		if accepted.EconomicCents+incomingEconomic > sale.TotalCents {
			return newFinancialConflict(
				"devolución rechazada: la venta sólo conserva %s reembolsables y el escritorio intentó aplicar %s",
				formatProjectorCents(maxInt64(sale.TotalCents-accepted.EconomicCents, 0)), formatProjectorCents(incomingEconomic),
			)
		}

		// Reconstruct the original debt from immutable rows, not from the
		// coalesced root.balance_pending snapshot. Refunding a settlement reopens
		// debt; ordinary product returns consume it. This makes two stale devices
		// compete for the same pending balance instead of both cancelling it.
		var reopenedCents int64
		if err := g.Raw(`SELECT COALESCE(SUM(ROUND(r.amount*100)::bigint),0)::bigint
			FROM refunds r
			JOIN payments rp ON rp.gym_id=r.gym_id AND rp.id=r.refund_payment_id AND rp.deleted_at IS NULL
			JOIN payments source ON source.gym_id=rp.gym_id AND source.id=rp.parent_payment_id
			  AND source.concept='balance_settlement' AND source.parent_payment_id=? AND source.deleted_at IS NULL
			WHERE r.gym_id=? AND r.root_payment_id=? AND r.id<>?
			  AND r.kind='revenue_refund' AND r.deleted_at IS NULL`,
			rootID, gymID, rootID, entityID).Scan(&reopenedCents).Error; err != nil {
			return fmt.Errorf("projector refunds: calculate reopened sale debt: %w", err)
		}
		initialPending := maxInt64(
			sale.TotalCents-(root.AmountCents+sums.SettlementsCents-reopenedCents), 0,
		)
		if accepted.CancelledCents+cancelledCents > initialPending {
			return newFinancialConflict(
				"devolución rechazada: sólo quedan %s de saldo por cancelar",
				formatProjectorCents(maxInt64(initialPending-accepted.CancelledCents, 0)),
			)
		}
		pending := initialPending - accepted.CancelledCents - cancelledCents
		reconciledPendingCents = &pending
	}
	if kind == "overcollection_settlement" {
		if cancelledCents != 0 || amountCents <= 0 || saleID == uuid.Nil {
			return newFinancialConflict("la devolución de un cobro excedente no coincide con una corrección de venta")
		}
		correctionID, parseErr := projectorUUID(raw, "correction_id")
		if parseErr != nil {
			return parseErr
		}
		var correction struct {
			SaleID        uuid.UUID `gorm:"column:sale_id"`
			PaymentID     uuid.UUID `gorm:"column:payment_id"`
			MonetaryDelta int64     `gorm:"column:monetary_delta_cents"`
		}
		correctionResult := g.Raw(`SELECT c.sale_id,s.payment_id,
			ROUND(c.monetary_delta*100)::bigint AS monetary_delta_cents
			FROM sale_corrections c JOIN sales s ON s.gym_id=c.gym_id AND s.id=c.sale_id
			WHERE c.gym_id=? AND c.id=? AND c.deleted_at IS NULL FOR UPDATE OF c`,
			gymID, correctionID).Scan(&correction)
		if correctionResult.Error != nil {
			return fmt.Errorf("projector refunds: lock sale correction: %w", correctionResult.Error)
		}
		if correctionResult.RowsAffected == 0 || correction.SaleID != saleID || correction.PaymentID != rootID {
			return newFinancialConflict("la corrección del cobro excedente no existe o pertenece a otra venta")
		}
		var settled int64
		if err := g.Raw(`SELECT COALESCE(SUM(ROUND(amount*100)::bigint),0)::bigint
			FROM refunds WHERE gym_id=? AND correction_id=? AND id<>?
			  AND kind='overcollection_settlement' AND deleted_at IS NULL`, gymID, correctionID, entityID).
			Scan(&settled).Error; err != nil {
			return fmt.Errorf("projector refunds: calculate correction settlement capacity: %w", err)
		}
		if settled+amountCents > correction.MonetaryDelta {
			return newFinancialConflict("devolución rechazada: el excedente de esa corrección ya fue liquidado")
		}
	}

	if amountCents > 0 {
		refundPaymentID, parseErr := projectorUUID(raw, "refund_payment_id")
		if parseErr != nil {
			return newFinancialConflict("devolución %s: falta el movimiento monetario vinculado", entityID)
		}
		var source struct {
			ParentID    uuid.UUID `gorm:"column:parent_id"`
			AmountCents int64     `gorm:"column:amount_cents"`
		}
		paymentResult := g.Raw(`SELECT parent_payment_id AS parent_id,
			ROUND(ABS(amount)*100)::bigint AS amount_cents
			FROM payments WHERE gym_id=? AND id=? AND concept='refund' AND amount<0
			  AND deleted_at IS NULL`, gymID, refundPaymentID).Scan(&source)
		if paymentResult.Error != nil {
			return fmt.Errorf("projector refunds: read refund payment: %w", paymentResult.Error)
		}
		if paymentResult.RowsAffected == 0 || source.AmountCents != amountCents {
			return newFinancialConflict("devolución %s: el movimiento monetario no coincide con el monto", entityID)
		}

		var sourceCapacity struct {
			AmountCents   int64      `gorm:"column:amount_cents"`
			Concept       string     `gorm:"column:concept"`
			ParentRootID  *uuid.UUID `gorm:"column:parent_root_id"`
			RefundedCents int64      `gorm:"column:refunded_cents"`
		}
		if err := g.Raw(`SELECT ROUND(source.amount*100)::bigint AS amount_cents,
			source.concept,source.parent_payment_id AS parent_root_id,
			COALESCE((SELECT SUM(ROUND(r.amount*100)::bigint)
			  FROM refunds r JOIN payments rp ON rp.id=r.refund_payment_id
			  WHERE r.gym_id=? AND r.id<>? AND r.deleted_at IS NULL
			    AND rp.parent_payment_id=source.id AND rp.deleted_at IS NULL),0)::bigint AS refunded_cents
			FROM payments source WHERE source.gym_id=? AND source.id=? AND source.deleted_at IS NULL`,
			gymID, entityID, gymID, source.ParentID).Scan(&sourceCapacity).Error; err != nil {
			return fmt.Errorf("projector refunds: calculate source balance: %w", err)
		}
		sourceBelongsToRoot := source.ParentID == rootID ||
			(sourceCapacity.Concept == "balance_settlement" && sourceCapacity.ParentRootID != nil && *sourceCapacity.ParentRootID == rootID)
		if !sourceBelongsToRoot || amountCents > sourceCapacity.AmountCents-sourceCapacity.RefundedCents {
			return newFinancialConflict("devolución rechazada: el cobro seleccionado ya no tiene saldo suficiente")
		}
	}

	if err := projectGeneric(g, table, gymID, entityID, payload); err != nil {
		return err
	}
	if reconciledPendingCents != nil {
		if err := reconcileProjectorPaymentBalance(g, gymID, rootID, *reconciledPendingCents); err != nil {
			return err
		}
	}
	return nil
}

// projectRefundItem makes the economic detail append-only and refuses to
// materialise a new line outside the same transaction as its parent Refund.
// Otherwise an old/split batch could first publish the aggregate and later
// discover that a product quantity was already consumed by another desk.
func projectRefundItem(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	raw, err := decodeProjectorPayload("refund_items", payload)
	if err != nil {
		return err
	}
	if raw["deleted_at"] != nil {
		return newFinancialConflict("un producto de una devolución no se puede eliminar ni reactivar")
	}
	refundID, refundErr := projectorUUID(raw, "refund_id")
	saleItemID, saleItemErr := projectorUUID(raw, "sale_item_id")
	quantity, quantityOK := numericInt(raw["quantity"])
	amountCents, amountErr := projectorMoneyCents(raw, "amount")
	disposition, dispositionOK := raw["disposition"].(string)
	if refundErr != nil || saleItemErr != nil || !quantityOK || quantity <= 0 || amountErr != nil || amountCents <= 0 ||
		!dispositionOK || (disposition != "returned_to_stock" && disposition != "damaged" && disposition != "not_returned") {
		return newFinancialConflict("el detalle de productos de la devolución es inválido")
	}

	var existing struct {
		RefundID    uuid.UUID `gorm:"column:refund_id"`
		SaleItemID  uuid.UUID `gorm:"column:sale_item_id"`
		Quantity    int       `gorm:"column:quantity"`
		AmountCents int64     `gorm:"column:amount_cents"`
		Disposition string    `gorm:"column:disposition"`
	}
	result := g.Raw(`SELECT refund_id,sale_item_id,quantity,
		ROUND(amount*100)::bigint AS amount_cents,disposition
		FROM refund_items WHERE gym_id=? AND id=? AND deleted_at IS NULL FOR UPDATE`,
		gymID, entityID).Scan(&existing)
	if result.Error != nil {
		return fmt.Errorf("projector refund_items: lock existing row: %w", result.Error)
	}
	if result.RowsAffected > 0 {
		if existing.RefundID != refundID || existing.SaleItemID != saleItemID ||
			existing.Quantity != quantity || existing.AmountCents != amountCents || existing.Disposition != disposition {
			return newFinancialConflict("el producto devuelto %s ya existe con otro efecto", entityID)
		}
		return projectGeneric(g, table, gymID, entityID, payload)
	}
	if !atomicRefundGraphActive(g) {
		return newFinancialConflict(
			"devolución incompleta: sus productos deben sincronizarse en el mismo grafo financiero",
		)
	}
	var links struct {
		RefundPresent bool `gorm:"column:refund_present"`
		LinePresent   bool `gorm:"column:line_present"`
	}
	if err := g.Raw(`SELECT
		EXISTS(SELECT 1 FROM refunds WHERE gym_id=? AND id=? AND deleted_at IS NULL) AS refund_present,
		EXISTS(SELECT 1 FROM sale_items WHERE gym_id=? AND id=? AND deleted_at IS NULL) AS line_present`,
		gymID, refundID, gymID, saleItemID).Scan(&links).Error; err != nil {
		return fmt.Errorf("projector refund_items: validate links: %w", err)
	}
	if !links.RefundPresent || !links.LinePresent {
		return newFinancialConflict("el producto devuelto no pertenece a un grafo vigente del gimnasio")
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

func atomicRefundGraphActive(g *gorm.DB) bool {
	var value string
	if err := g.Raw(`SELECT COALESCE(current_setting('tinta.atomic_refund_graph', true),'')`).Scan(&value).Error; err != nil {
		return false
	}
	return value == "1"
}

// reconcileProjectorPaymentBalance repairs the mutable debt snapshot after an
// accepted product refund. The sidecar necessarily enqueues the root Payment
// before the immutable Refund rows and coalesces repeated root updates, so the
// LWW payload may represent only one desk's cancellation. Bump both the domain
// row and its journal entry when the immutable aggregate proves a different
// canonical balance; the next pull then repairs every sidecar too.
func reconcileProjectorPaymentBalance(g *gorm.DB, gymID, paymentID uuid.UUID, pendingCents int64) error {
	result := g.Exec(`UPDATE payments
		SET balance_pending=?::numeric/100,version=version+1,updated_at=NOW()
		WHERE gym_id=? AND id=? AND deleted_at IS NULL
		  AND ROUND(balance_pending*100)::bigint<>?`, pendingCents, gymID, paymentID, pendingCents)
	if result.Error != nil {
		return fmt.Errorf("projector refunds: reconcile root balance: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil
	}
	if err := g.Exec(`UPDATE sync_entities se
		SET version=GREATEST(se.version+1,p.version),
			payload=jsonb_set(
				jsonb_set(
					jsonb_set(se.payload,'{balance_pending}',to_jsonb(p.balance_pending),TRUE),
					'{version}',to_jsonb(GREATEST(se.version+1,p.version)),TRUE),
				'{updated_at}',to_jsonb((EXTRACT(EPOCH FROM p.updated_at)*1000)::bigint),TRUE),
			server_updated_at=NOW()
		FROM payments p
		WHERE se.gym_id=? AND se.entity_type='payments' AND se.entity_id=?
		  AND p.gym_id=se.gym_id AND p.id=se.entity_id`, gymID, paymentID).Error; err != nil {
		return fmt.Errorf("projector refunds: publish reconciled root balance: %w", err)
	}
	return nil
}

// projectPaymentCorrection serialises administrative payment rewrites on the
// Payment row and allows exactly one immutable transition per expected
// version. This is the sync-side equivalent of the optimistic UPDATE used by
// the command layer; distinct UUIDs from two offline desks must still compete
// for the same version slot.
func projectPaymentCorrection(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	raw, err := decodeProjectorPayload("payment_corrections", payload)
	if err != nil {
		return err
	}
	if raw["deleted_at"] != nil {
		return newFinancialConflict("una corrección administrativa de cobro no se puede eliminar")
	}
	paymentID, err := projectorUUID(raw, "payment_id")
	if err != nil {
		return err
	}
	expected, ok := numericInt(raw["expected_payment_version"])
	if !ok || expected <= 0 {
		return newFinancialConflict("corrección de cobro %s: versión esperada inválida", entityID)
	}
	reason, reasonOK := raw["reason"].(string)
	key, keyOK := raw["idempotency_key"].(string)
	fingerprint, fingerprintOK := raw["idempotency_fingerprint"].(string)
	createdBy, createdByErr := projectorUUID(raw, "created_by")
	if !reasonOK || strings.TrimSpace(reason) != reason || len(reason) < 3 || len(reason) > 200 ||
		!keyOK || strings.TrimSpace(key) != key || key == "" || len(key) > 120 ||
		!fingerprintOK || strings.TrimSpace(fingerprint) != fingerprint || fingerprint == "" || len(fingerprint) > 128 || createdByErr != nil {
		return newFinancialConflict("corrección de cobro %s: evidencia administrativa inválida", entityID)
	}
	var author struct {
		Present bool `gorm:"column:present"`
	}
	if result := g.Raw(`SELECT EXISTS(
		SELECT 1 FROM users WHERE gym_id=? AND id=? AND deleted_at IS NULL
	) AS present`, gymID, createdBy).Scan(&author); result.Error != nil {
		return fmt.Errorf("projector payment_corrections: validate author: %w", result.Error)
	}
	if !author.Present {
		return newFinancialConflict("corrección de cobro %s: el autor no pertenece al gimnasio", entityID)
	}

	var payment struct {
		Version         int        `gorm:"column:version"`
		Concept         string     `gorm:"column:concept"`
		ParentID        *uuid.UUID `gorm:"column:parent_id"`
		AmountCents     int64      `gorm:"column:amount_cents"`
		RecognizedCents int64      `gorm:"column:recognized_cents"`
		PendingCents    int64      `gorm:"column:pending_cents"`
		Method          string     `gorm:"column:payment_method"`
		CashDrawerID    *uuid.UUID `gorm:"column:cash_drawer_id"`
		CashDestination string     `gorm:"column:cash_destination"`
		PaymentDate     string     `gorm:"column:payment_date"`
		Deleted         bool       `gorm:"column:deleted"`
	}
	paymentResult := g.Raw(`SELECT version,concept,parent_payment_id AS parent_id,
		ROUND(amount*100)::bigint AS amount_cents,
		ROUND(recognized_amount*100)::bigint AS recognized_cents,
		ROUND(balance_pending*100)::bigint AS pending_cents,
		payment_method,cash_drawer_id,cash_destination,payment_date::text AS payment_date,
		(deleted_at IS NOT NULL) AS deleted
		FROM payments WHERE gym_id=? AND id=? FOR UPDATE`, gymID, paymentID).Scan(&payment)
	if paymentResult.Error != nil {
		return fmt.Errorf("projector payment_corrections: lock payment: %w", paymentResult.Error)
	}
	if paymentResult.RowsAffected == 0 || payment.ParentID != nil ||
		(payment.Concept != "membership" && payment.Concept != "other") {
		return newFinancialConflict("el cobro indicado no admite corrección administrativa")
	}

	var competing struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	competingResult := g.Raw(`SELECT id FROM payment_corrections
		WHERE gym_id=? AND payment_id=? AND expected_payment_version=? AND id<>?
		  AND deleted_at IS NULL LIMIT 1 FOR UPDATE`, gymID, paymentID, expected, entityID).Scan(&competing)
	if competingResult.Error != nil {
		return fmt.Errorf("projector payment_corrections: find competing version: %w", competingResult.Error)
	}
	if competingResult.RowsAffected > 0 && competing.ID != uuid.Nil {
		return newFinancialConflict(
			"corrección rechazada: otro escritorio ya corrigió la versión %d de este cobro", expected,
		)
	}
	if payment.Version < expected+1 {
		return newFinancialConflict(
			"corrección rechazada: el cobro está en versión %d pero el escritorio corrigió desde %d",
			payment.Version, expected,
		)
	}

	before, beforeErr := projectorSnapshot(raw["before_snapshot"])
	after, afterErr := projectorSnapshot(raw["after_snapshot"])
	beforeState, beforeStateErr := projectorAdministrativePaymentSnapshot(before)
	afterState, afterStateErr := projectorAdministrativePaymentSnapshot(after)
	if beforeErr != nil || afterErr != nil || beforeStateErr != nil || afterStateErr != nil ||
		beforeState.Version != expected || afterState.Version != expected+1 || beforeState.Annulled {
		return newFinancialConflict("corrección rechazada: el historial del cobro es inválido")
	}
	if afterState.Annulled {
		if payment.Concept != "other" || !sameProjectorAdministrativePaymentFacts(beforeState, afterState) {
			return newFinancialConflict("corrección rechazada: la anulación no coincide con un ingreso extraordinario intacto")
		}
	} else if sameProjectorAdministrativePaymentFacts(beforeState, afterState) {
		return newFinancialConflict("corrección rechazada: el historial no contiene ningún cambio")
	}
	if payment.Concept == "other" && afterState.PendingCents != 0 {
		return newFinancialConflict("un ingreso extraordinario no puede conservar saldo pendiente")
	}
	if (afterState.Method == "cash" && afterState.CashDestination != "gym_fund" && afterState.CashDrawerID == nil) ||
		((afterState.Method != "cash" || afterState.CashDestination == "gym_fund") && afterState.CashDrawerID != nil) {
		return newFinancialConflict("corrección rechazada: la caja no coincide con el método de pago")
	}

	// The evidence row is append-only. A replay may finalize its result JSON,
	// but cannot change the payment/version slot, snapshots or authorship.
	var existing struct {
		PaymentID   uuid.UUID       `gorm:"column:payment_id"`
		Expected    int             `gorm:"column:expected_payment_version"`
		Reason      string          `gorm:"column:reason"`
		Before      json.RawMessage `gorm:"column:before_snapshot"`
		After       json.RawMessage `gorm:"column:after_snapshot"`
		Key         string          `gorm:"column:idempotency_key"`
		Fingerprint string          `gorm:"column:idempotency_fingerprint"`
		CreatedBy   uuid.UUID       `gorm:"column:created_by"`
	}
	existingResult := g.Raw(`SELECT payment_id,expected_payment_version,reason,before_snapshot,after_snapshot,
		idempotency_key,idempotency_fingerprint,created_by
		FROM payment_corrections WHERE gym_id=? AND id=? AND deleted_at IS NULL FOR UPDATE`,
		gymID, entityID).Scan(&existing)
	if existingResult.Error != nil {
		return fmt.Errorf("projector payment_corrections: lock existing correction: %w", existingResult.Error)
	}
	if existingResult.RowsAffected > 0 {
		existingBefore, existingBeforeErr := projectorSnapshot(existing.Before)
		existingAfter, existingAfterErr := projectorSnapshot(existing.After)
		if existingBeforeErr != nil || existingAfterErr != nil || existing.PaymentID != paymentID ||
			existing.Expected != expected || existing.Reason != reason || existing.Key != key ||
			existing.Fingerprint != fingerprint || existing.CreatedBy != createdBy ||
			!sameProjectorSnapshot(existingBefore, before) || !sameProjectorSnapshot(existingAfter, after) {
			return newFinancialConflict("la corrección de cobro %s ya existe con otra evidencia", entityID)
		}
	}

	// Consecutive offline corrections are queued as immutable rows while the
	// Payment payload coalesces to its final version. When transitions are
	// adjacent, require exact snapshot continuity. Version gaps are allowed
	// because settlements and other normal commands may advance Payment.version
	// between administrative corrections.
	var predecessor struct {
		Expected int             `gorm:"column:expected_payment_version"`
		After    json.RawMessage `gorm:"column:after_snapshot"`
	}
	predecessorResult := g.Raw(`SELECT expected_payment_version,after_snapshot FROM payment_corrections
		WHERE gym_id=? AND payment_id=? AND expected_payment_version<? AND deleted_at IS NULL
		ORDER BY expected_payment_version DESC LIMIT 1`, gymID, paymentID, expected).Scan(&predecessor)
	if predecessorResult.Error != nil {
		return fmt.Errorf("projector payment_corrections: read predecessor: %w", predecessorResult.Error)
	}
	if predecessorResult.RowsAffected > 0 && predecessor.Expected == expected-1 {
		previousAfter, parseErr := projectorSnapshot(predecessor.After)
		if parseErr != nil || !sameProjectorSnapshot(previousAfter, before) {
			return newFinancialConflict("corrección rechazada: el historial no continúa desde la corrección anterior")
		}
	}
	if payment.Version == expected+1 {
		canonical := projectorAdminPaymentSnapshot{CashDestination: payment.CashDestination, Version: payment.Version, AmountCents: payment.AmountCents,
			RecognizedCents: payment.RecognizedCents, PendingCents: payment.PendingCents, Method: payment.Method,
			CashDrawerID: payment.CashDrawerID, PaymentDate: payment.PaymentDate, Annulled: payment.Deleted}
		if !sameProjectorAdministrativePaymentSnapshot(canonical, afterState) {
			return newFinancialConflict("corrección rechazada: el resultado no coincide con el cobro vigente")
		}
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

// projectSaleCorrection accepts exactly one correction for a sale revision.
// The Sale row is locked, so two sidecars that both corrected version N
// cannot both append an N->N+1 audit record even though their correction IDs
// differ.
func projectSaleCorrection(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	raw, err := decodeProjectorPayload("sale_corrections", payload)
	if err != nil {
		return err
	}
	if raw["deleted_at"] != nil {
		return newFinancialConflict("una corrección de venta sincronizada no se puede eliminar")
	}
	saleID, err := projectorUUID(raw, "sale_id")
	if err != nil {
		return err
	}
	expected, ok := numericInt(raw["expected_sale_version"])
	if !ok || expected < 0 {
		return newFinancialConflict("corrección %s: versión esperada inválida", entityID)
	}
	correctionType, _ := raw["correction_type"].(string)
	if correctionType == "" {
		correctionType = "edit"
	}
	if correctionType != "edit" && correctionType != "annul" {
		return newFinancialConflict("corrección %s: tipo inválido", entityID)
	}
	resolution, _ := raw["money_resolution"].(string)
	if resolution != "record_only" && resolution != "refund_excess" && resolution != "refund_pending" {
		return newFinancialConflict("corrección %s: resolución monetaria inválida", entityID)
	}
	increase, _ := raw["increase_resolution"].(string)
	if increase != "" && increase != "pending" && increase != "already_collected" && increase != "collect_now" {
		return newFinancialConflict("corrección %s: resolución de incremento inválida", entityID)
	}
	if correctionType == "annul" && increase != "" {
		return newFinancialConflict("corrección %s: una anulación no puede incrementar la venta", entityID)
	}

	var link struct {
		PaymentID uuid.UUID `gorm:"column:payment_id"`
	}
	linkResult := g.Raw(`SELECT payment_id FROM sales WHERE gym_id=? AND id=?`,
		gymID, saleID).Scan(&link)
	if linkResult.Error != nil {
		return fmt.Errorf("projector sale_corrections: read sale link: %w", linkResult.Error)
	}
	if linkResult.RowsAffected == 0 {
		return newFinancialConflict("la venta de la corrección %s no existe en la nube", entityID)
	}
	var lockedPayment struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	paymentLock := g.Raw(`SELECT id FROM payments WHERE gym_id=? AND id=? FOR UPDATE`,
		gymID, link.PaymentID).Scan(&lockedPayment)
	if paymentLock.Error != nil {
		return fmt.Errorf("projector sale_corrections: lock sale payment: %w", paymentLock.Error)
	}
	if paymentLock.RowsAffected == 0 {
		return newFinancialConflict("el cobro de la venta corregida no existe en la nube")
	}

	var sale struct {
		PaymentID         uuid.UUID `gorm:"column:payment_id"`
		CorrectionVersion int       `gorm:"column:correction_version"`
		SubtotalCents     int64     `gorm:"column:subtotal_cents"`
		DiscountCents     int64     `gorm:"column:discount_cents"`
		TotalCents        int64     `gorm:"column:total_cents"`
		Deleted           bool      `gorm:"column:deleted"`
	}
	saleResult := g.Raw(`SELECT payment_id,correction_version,
		ROUND(subtotal*100)::bigint AS subtotal_cents,
		ROUND(discount*100)::bigint AS discount_cents,
		ROUND(total*100)::bigint AS total_cents,
		(deleted_at IS NOT NULL) AS deleted
		FROM sales WHERE gym_id=? AND id=? FOR UPDATE`, gymID, saleID).Scan(&sale)
	if saleResult.Error != nil {
		return fmt.Errorf("projector sale_corrections: lock sale: %w", saleResult.Error)
	}
	if saleResult.RowsAffected == 0 {
		return newFinancialConflict("la venta de la corrección %s no existe en la nube", entityID)
	}
	if sale.PaymentID != link.PaymentID {
		return newFinancialConflict("la venta cambió de cobro mientras se sincronizaba la corrección")
	}

	var competing struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	competingResult := g.Raw(`SELECT id FROM sale_corrections
		WHERE gym_id=? AND sale_id=? AND expected_sale_version=? AND id<>?
		  AND deleted_at IS NULL LIMIT 1 FOR UPDATE`, gymID, saleID, expected, entityID).Scan(&competing)
	if competingResult.Error != nil {
		return fmt.Errorf("projector sale_corrections: find competing revision: %w", competingResult.Error)
	}
	if competingResult.RowsAffected > 0 && competing.ID != uuid.Nil {
		return newFinancialConflict(
			"corrección rechazada: otro escritorio ya corrigió la versión %d de esta venta", expected,
		)
	}
	if sale.CorrectionVersion < expected+1 {
		return newFinancialConflict(
			"corrección rechazada: la venta está en versión %d pero el escritorio envió una transición desde %d",
			sale.CorrectionVersion, expected,
		)
	}

	before, err := projectorSnapshot(raw["before_snapshot"])
	if err != nil {
		return newFinancialConflict("corrección %s: snapshot anterior inválido", entityID)
	}
	after, err := projectorSnapshot(raw["after_snapshot"])
	if err != nil {
		return newFinancialConflict("corrección %s: snapshot posterior inválido", entityID)
	}
	beforeVersion, beforeOK := numericInt(before["correction_version"])
	afterVersion, afterOK := numericInt(after["correction_version"])
	afterSubtotal, subtotalOK := projectorIntegralInt64(after["subtotal_cents"])
	afterDiscount, discountOK := projectorIntegralInt64(after["discount_cents"])
	afterTotal, totalOK := projectorIntegralInt64(after["total_cents"])
	if !beforeOK || !afterOK || !subtotalOK || !discountOK || !totalOK ||
		beforeVersion != expected || afterVersion != expected+1 {
		return newFinancialConflict("corrección rechazada: el historial no coincide con el estado vigente de la venta")
	}

	// The audit row is immutable even if an old sidecar later presents a higher
	// row version for the same UUID. Idempotency result may be finalized after
	// creation, but the fingerprint, resolution and before/after effect may not
	// be rewritten.
	var existing struct {
		SaleID         uuid.UUID       `gorm:"column:sale_id"`
		Expected       int             `gorm:"column:expected_sale_version"`
		CorrectionType string          `gorm:"column:correction_type"`
		Resolution     string          `gorm:"column:money_resolution"`
		Increase       string          `gorm:"column:increase_resolution"`
		DeltaCents     int64           `gorm:"column:monetary_delta_cents"`
		Before         json.RawMessage `gorm:"column:before_snapshot"`
		After          json.RawMessage `gorm:"column:after_snapshot"`
		Key            string          `gorm:"column:idempotency_key"`
		Fingerprint    string          `gorm:"column:idempotency_fingerprint"`
	}
	existingResult := g.Raw(`SELECT sale_id,expected_sale_version,correction_type,money_resolution,
		COALESCE(increase_resolution,'') AS increase_resolution,
		ROUND(monetary_delta*100)::bigint AS monetary_delta_cents,
		before_snapshot,after_snapshot,idempotency_key,idempotency_fingerprint
		FROM sale_corrections WHERE gym_id=? AND id=? AND deleted_at IS NULL FOR UPDATE`,
		gymID, entityID).Scan(&existing)
	if existingResult.Error != nil {
		return fmt.Errorf("projector sale_corrections: lock existing correction: %w", existingResult.Error)
	}
	if existingResult.RowsAffected > 0 {
		deltaCents, deltaErr := projectorSignedMoneyCents(raw, "monetary_delta")
		key, _ := raw["idempotency_key"].(string)
		fingerprint, _ := raw["idempotency_fingerprint"].(string)
		existingBefore, beforeErr := projectorSnapshot(existing.Before)
		existingAfter, afterErr := projectorSnapshot(existing.After)
		if deltaErr != nil || beforeErr != nil || afterErr != nil ||
			existing.SaleID != saleID || existing.Expected != expected ||
			existing.CorrectionType != correctionType || existing.Resolution != resolution || existing.Increase != increase ||
			existing.DeltaCents != deltaCents || existing.Key != key || existing.Fingerprint != fingerprint ||
			!sameProjectorSnapshot(existingBefore, before) || !sameProjectorSnapshot(existingAfter, after) {
			return newFinancialConflict("la corrección %s ya existe con otro efecto financiero", entityID)
		}
	}

	// Several corrections may be captured offline before the first sync. The
	// mutable Sale queue row is coalesced to the final version, while immutable
	// correction rows remain one per transition. Accept that chain in order,
	// requiring every predecessor and exact snapshot continuity. Only the last
	// transition must match the already-coalesced Sale totals.
	if expected > 0 {
		var priorCount int64
		if err := g.Raw(`SELECT COUNT(DISTINCT expected_sale_version)
			FROM sale_corrections WHERE gym_id=? AND sale_id=?
			  AND expected_sale_version>=0 AND expected_sale_version<? AND deleted_at IS NULL`,
			gymID, saleID, expected).Scan(&priorCount).Error; err != nil {
			return fmt.Errorf("projector sale_corrections: count predecessor chain: %w", err)
		}
		if priorCount != int64(expected) {
			return newFinancialConflict("corrección rechazada: falta una corrección anterior de esta venta")
		}
		var predecessor struct {
			AfterSnapshot json.RawMessage `gorm:"column:after_snapshot"`
		}
		predecessorResult := g.Raw(`SELECT after_snapshot FROM sale_corrections
			WHERE gym_id=? AND sale_id=? AND expected_sale_version=? AND deleted_at IS NULL
			LIMIT 1`, gymID, saleID, expected-1).Scan(&predecessor)
		if predecessorResult.Error != nil {
			return fmt.Errorf("projector sale_corrections: read predecessor snapshot: %w", predecessorResult.Error)
		}
		previousAfter, parseErr := projectorSnapshot(predecessor.AfterSnapshot)
		if predecessorResult.RowsAffected == 0 || parseErr != nil || !sameProjectorSnapshot(previousAfter, before) {
			return newFinancialConflict("corrección rechazada: el historial no continúa desde la corrección anterior")
		}
	}
	if sale.CorrectionVersion == expected+1 {
		wantSaleDeleted := correctionType == "annul"
		wantPaymentDeleted := correctionType == "annul" && resolution == "record_only"
		if sale.Deleted != wantSaleDeleted {
			return newFinancialConflict("corrección rechazada: el estado activo/anulado no coincide con su tipo")
		}
		if afterSubtotal != sale.SubtotalCents || afterDiscount != sale.DiscountCents || afterTotal != sale.TotalCents {
			return newFinancialConflict("corrección rechazada: el resultado no coincide con la venta vigente")
		}
		if snapshotAnnulled, present := after["annulled"].(bool); present &&
			(snapshotAnnulled != sale.Deleted || snapshotAnnulled != wantSaleDeleted) {
			return newFinancialConflict("corrección rechazada: el snapshot no coincide con el estado anulado de la venta")
		}
		wantPayment, hasPayment := projectorIntegralInt64(after["payment_amount_cents"])
		wantRecognized, hasRecognized := projectorIntegralInt64(after["recognized_amount_cents"])
		_, hasPaymentTombstone := after["payment_tombstoned"].(bool)
		if hasPayment || hasRecognized || hasPaymentTombstone {
			var payment struct {
				AmountCents     int64 `gorm:"column:amount_cents"`
				RecognizedCents int64 `gorm:"column:recognized_cents"`
				Deleted         bool  `gorm:"column:deleted"`
			}
			paymentResult := g.Raw(`SELECT ROUND(amount*100)::bigint AS amount_cents,
				ROUND(recognized_amount*100)::bigint AS recognized_cents,
				(deleted_at IS NOT NULL) AS deleted
				FROM payments WHERE gym_id=? AND id=?`, gymID, sale.PaymentID).Scan(&payment)
			if paymentResult.Error != nil {
				return fmt.Errorf("projector sale_corrections: verify corrected payment: %w", paymentResult.Error)
			}
			if paymentResult.RowsAffected == 0 {
				return newFinancialConflict("corrección rechazada: el cobro corregido no existe")
			}
			if (hasPayment && payment.AmountCents != wantPayment) ||
				(hasRecognized && payment.RecognizedCents != wantRecognized) {
				return newFinancialConflict("corrección rechazada: el cobro no coincide con el resultado corregido")
			}
			if wantDeleted, present := after["payment_tombstoned"].(bool); present && payment.Deleted != wantDeleted {
				return newFinancialConflict("corrección rechazada: el estado activo/anulado del cobro no coincide con el snapshot")
			}
			if payment.Deleted != wantPaymentDeleted {
				return newFinancialConflict("corrección rechazada: el estado del cobro no coincide con la resolución monetaria")
			}
		}
		lines, hasLines, linesErr := projectorSaleLines(after["lines"])
		if linesErr != nil {
			return newFinancialConflict("corrección rechazada: el detalle posterior de productos es inválido")
		}
		if hasLines {
			matches, matchErr := projectorCanonicalSaleLinesMatch(g, gymID, saleID, lines)
			if matchErr != nil {
				return fmt.Errorf("projector sale_corrections: verify corrected sale items: %w", matchErr)
			}
			if !matches {
				return newFinancialConflict("corrección rechazada: los productos no coinciden con el resultado corregido")
			}
		}
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

func decodeProjectorPayload(entityType string, payload []byte) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("projector %s: payload not a JSON object: %w", entityType, err)
	}
	return raw, nil
}

func projectorUUID(raw map[string]any, field string) (uuid.UUID, error) {
	value, _ := raw[field].(string)
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, newFinancialConflict("campo financiero %s inválido", field)
	}
	return id, nil
}

func projectorOptionalUUID(v any) *uuid.UUID {
	value, _ := v.(string)
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return nil
	}
	return &id
}

func sameProjectorOptionalUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func projectorMoneyCents(raw map[string]any, field string) (int64, error) {
	cents, err := projectorSignedMoneyCents(raw, field)
	if err != nil || cents < 0 {
		return 0, newFinancialConflict("campo financiero %s inválido", field)
	}
	return cents, nil
}

func projectorSignedMoneyCents(raw map[string]any, field string) (int64, error) {
	v, ok := raw[field]
	if !ok || v == nil {
		return 0, newFinancialConflict("campo financiero %s ausente", field)
	}
	var value float64
	switch n := v.(type) {
	case float64:
		value = n
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, err
		}
		value = f
	case int:
		value = float64(n)
	case int64:
		value = float64(n)
	default:
		return 0, newFinancialConflict("campo financiero %s inválido", field)
	}
	scaled := value * 100
	if math.IsNaN(value) || math.IsInf(value, 0) ||
		math.Abs(scaled-math.Round(scaled)) > 1e-7 ||
		math.Abs(scaled) > float64(math.MaxInt64) {
		return 0, newFinancialConflict("campo financiero %s inválido", field)
	}
	return int64(math.Round(scaled)), nil
}

func projectorSnapshot(v any) (map[string]any, error) {
	var raw map[string]any
	switch value := v.(type) {
	case map[string]any:
		return value, nil
	case string:
		if err := json.Unmarshal([]byte(value), &raw); err != nil {
			return nil, err
		}
		return raw, nil
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
}

func projectorSaleLines(v any) (map[uuid.UUID]projectorSaleLine, bool, error) {
	if v == nil {
		return nil, false, nil
	}
	var values []any
	switch typed := v.(type) {
	case []any:
		values = typed
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, true, err
		}
		if err := json.Unmarshal(encoded, &values); err != nil {
			return nil, true, err
		}
	}
	lines := make(map[uuid.UUID]projectorSaleLine, len(values))
	for _, value := range values {
		row, ok := value.(map[string]any)
		if !ok {
			return nil, true, fmt.Errorf("sale line is %T", value)
		}
		itemID, err := projectorUUID(row, "sale_item_id")
		if err != nil {
			return nil, true, err
		}
		productID, err := projectorUUID(row, "product_id")
		if err != nil {
			return nil, true, err
		}
		name, nameOK := row["product_name"].(string)
		unitPrice, priceOK := projectorIntegralInt64(row["unit_price_cents"])
		quantity, quantityOK := numericInt(row["quantity"])
		lineTotal, totalOK := projectorIntegralInt64(row["line_total_cents"])
		if !nameOK || !priceOK || !quantityOK || !totalOK || unitPrice < 0 || quantity <= 0 || lineTotal < 0 {
			return nil, true, fmt.Errorf("invalid sale line %s", itemID)
		}
		var unitCost *int64
		if row["unit_cost_cents"] != nil {
			cost, costOK := projectorIntegralInt64(row["unit_cost_cents"])
			if !costOK || cost < 0 {
				return nil, true, fmt.Errorf("invalid sale line cost %s", itemID)
			}
			unitCost = &cost
		}
		if _, duplicate := lines[itemID]; duplicate {
			return nil, true, fmt.Errorf("duplicate sale line %s", itemID)
		}
		lines[itemID] = projectorSaleLine{SaleItemID: itemID, ProductID: productID, ProductName: name,
			UnitPriceCents: unitPrice, UnitCostCents: unitCost, Quantity: quantity, LineTotalCents: lineTotal}
	}
	return lines, true, nil
}

func projectorSaleLineMatchesPayload(want projectorSaleLine, raw map[string]any) bool {
	productID, err := projectorUUID(raw, "product_id")
	if err != nil || productID != want.ProductID {
		return false
	}
	name, _ := raw["product_name_snapshot"].(string)
	unitPrice, priceErr := projectorMoneyCents(raw, "unit_price_snapshot")
	quantity, quantityOK := numericInt(raw["quantity"])
	lineTotal, totalErr := projectorMoneyCents(raw, "line_total")
	if name != want.ProductName || priceErr != nil || unitPrice != want.UnitPriceCents ||
		!quantityOK || quantity != want.Quantity || totalErr != nil || lineTotal != want.LineTotalCents {
		return false
	}
	if want.UnitCostCents == nil {
		return raw["unit_cost_snapshot"] == nil
	}
	unitCost, costErr := projectorMoneyCents(raw, "unit_cost_snapshot")
	return costErr == nil && unitCost == *want.UnitCostCents
}

func projectorCanonicalSaleLinesMatch(
	g *gorm.DB,
	gymID, saleID uuid.UUID,
	want map[uuid.UUID]projectorSaleLine,
) (bool, error) {
	var rows []struct {
		ID             uuid.UUID `gorm:"column:id"`
		ProductID      uuid.UUID `gorm:"column:product_id"`
		ProductName    string    `gorm:"column:product_name"`
		UnitPriceCents int64     `gorm:"column:unit_price_cents"`
		UnitCostCents  int64     `gorm:"column:unit_cost_cents"`
		Quantity       int       `gorm:"column:quantity"`
		LineTotalCents int64     `gorm:"column:line_total_cents"`
	}
	if err := g.Raw(`SELECT id,product_id,product_name_snapshot AS product_name,
		ROUND(unit_price_snapshot*100)::bigint AS unit_price_cents,
		COALESCE(ROUND(unit_cost_snapshot*100)::bigint,-1) AS unit_cost_cents,
		quantity,ROUND(line_total*100)::bigint AS line_total_cents
		FROM sale_items WHERE gym_id=? AND sale_id=? AND deleted_at IS NULL`, gymID, saleID).Scan(&rows).Error; err != nil {
		return false, err
	}
	if len(rows) != len(want) {
		return false, nil
	}
	for _, row := range rows {
		expected, ok := want[row.ID]
		if !ok || expected.ProductID != row.ProductID || expected.ProductName != row.ProductName ||
			expected.UnitPriceCents != row.UnitPriceCents || expected.Quantity != row.Quantity ||
			expected.LineTotalCents != row.LineTotalCents {
			return false, nil
		}
		if (expected.UnitCostCents == nil && row.UnitCostCents != -1) ||
			(expected.UnitCostCents != nil && *expected.UnitCostCents != row.UnitCostCents) {
			return false, nil
		}
	}
	return true, nil
}

func sameProjectorSnapshot(a, b map[string]any) bool {
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && string(aJSON) == string(bJSON)
}

func projectorIntegralInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		i := int64(n)
		return i, n == float64(i)
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

func projectorEpochMS(v any) (int64, bool) { return projectorIntegralInt64(v) }

func formatProjectorCents(cents int64) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func projectCashSession(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	normalized, raw, err := normalizeCashSessionPayload(gymID, payload)
	if err != nil {
		return fmt.Errorf("projector cash_close_events: %w", err)
	}
	if raw["deleted_at"] == nil {
		drawerID, _ := raw["drawer_id"].(string)
		operationalDate, _ := raw["operational_date"].(string)
		sequence, _ := numericInt(raw["sequence"])
		if drawerID != "" && operationalDate != "" && sequence > 0 {
			var collisions []struct{ ID uuid.UUID }
			if err := g.Raw(`SELECT id FROM cash_close_events
				WHERE gym_id=? AND drawer_id=? AND operational_date=? AND sequence=?
				  AND id<>? AND deleted_at IS NULL FOR UPDATE`,
				gymID, drawerID, operationalDate, sequence, entityID).Scan(&collisions).Error; err != nil {
				return fmt.Errorf("projector cash session: lock natural slot: %w", err)
			}
			if len(collisions) > 0 {
				now := time.Now().UTC()
				for _, c := range collisions {
					if err := g.Exec(`UPDATE cash_close_events
						SET deleted_at=?,updated_at=?,version=version+1 WHERE gym_id=? AND id=? AND deleted_at IS NULL`,
						now, now, gymID, c.ID).Error; err != nil {
						return fmt.Errorf("projector cash session: tombstone legacy slot: %w", err)
					}
					if err := g.Exec(`UPDATE sync_entities
						SET version=version+1,deleted_at=?,server_updated_at=?,
						    payload=jsonb_set(jsonb_set(payload,'{deleted_at}',to_jsonb(?::bigint),true),'{updated_at}',to_jsonb(?::bigint),true)
						WHERE gym_id=? AND entity_type='cash_close_events' AND entity_id=?`,
						now, now, now.UnixMilli(), now.UnixMilli(), gymID, c.ID).Error; err != nil {
						return fmt.Errorf("projector cash session: tombstone legacy journal: %w", err)
					}
				}
			}
		}
	}
	return projectGeneric(g, table, gymID, entityID, normalized)
}

// normalizeCashSessionPayload is the one-version compatibility alias for old
// sidecars. Missing session fields are reconstructed conservatively: the old
// calculated amount becomes activity with opening zero, but
// opening_cash_known=false prevents claiming complete reconciliation.
func normalizeCashSessionPayload(gymID uuid.UUID, payload []byte) ([]byte, map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, nil, fmt.Errorf("payload not a JSON object: %w", err)
	}
	if _, ok := raw["drawer_id"]; !ok {
		raw["drawer_id"] = gymID.String()
	}
	if _, ok := raw["drawer_code"]; !ok {
		raw["drawer_code"] = "main"
	}
	if _, ok := raw["operational_date"]; !ok {
		raw["operational_date"] = raw["close_date"]
	}
	if _, ok := raw["sequence"]; !ok {
		raw["sequence"] = 1
	}
	if _, ok := raw["opening_cash"]; !ok {
		raw["opening_cash"] = 0
	}
	if _, ok := raw["opening_cash_known"]; !ok {
		raw["opening_cash_known"] = false
	}
	if _, ok := raw["activity_cash"]; !ok {
		raw["activity_cash"] = raw["calculated_cash"]
	}
	if _, ok := raw["adjusted_after_withdrawal"]; !ok {
		raw["adjusted_after_withdrawal"] = false
	}
	// SQLite persists booleans as INTEGER and json_object therefore emits
	// 0/1. pgx intentionally refuses to encode those numbers into BOOLEAN;
	// normalize at the wire boundary before the generic projector builds its
	// argument list. Both fields are write barriers, so values outside 0/1
	// are rejected instead of being treated as truthy.
	for _, key := range []string{"opening_cash_known", "adjusted_after_withdrawal"} {
		if err := normalizeProjectorBoolean(raw, key); err != nil {
			return nil, nil, err
		}
	}
	if _, ok := raw["opened_at"]; !ok {
		raw["opened_at"] = legacyCashSessionDayStart(raw)
	}
	if _, ok := raw["opened_by"]; !ok {
		raw["opened_by"] = raw["closed_by"]
	}
	if _, ok := raw["closed_at"]; !ok {
		raw["closed_at"] = raw["created_at"]
	}
	if _, ok := raw["status"]; !ok {
		if raw["counted_cash"] == nil {
			raw["status"] = "closed_unverified"
		} else {
			raw["status"] = "reconciled"
		}
	}
	if raw["counted_cash"] != nil {
		if _, ok := raw["reconciled_at"]; !ok {
			raw["reconciled_at"] = raw["closed_at"]
		}
		if _, ok := raw["reconciled_by"]; !ok {
			raw["reconciled_by"] = raw["closed_by"]
		}
	}
	normalized, err := json.Marshal(raw)
	return normalized, raw, err
}

func normalizeProjectorBoolean(raw map[string]any, key string) error {
	value, present := raw[key]
	if !present || value == nil {
		return nil
	}
	if _, ok := value.(bool); ok {
		return nil
	}
	n, ok := numericInt(value)
	if !ok || (n != 0 && n != 1) {
		return fmt.Errorf("field %s must be boolean or 0/1", key)
	}
	raw[key] = n == 1
	return nil
}

func legacyCashSessionDayStart(raw map[string]any) any {
	for _, key := range []string{"operational_date", "close_date"} {
		if value, ok := raw[key].(string); ok && len(value) >= len("2006-01-02") {
			if parsed, err := time.Parse("2006-01-02", value[:len("2006-01-02")]); err == nil {
				return parsed.UTC().UnixMilli()
			}
		}
	}
	if raw["created_at"] != nil {
		return raw["created_at"]
	}
	return raw["updated_at"]
}

func numericInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n == float64(int(n))
	case int:
		return n, true
	case json.Number:
		i, err := strconv.Atoi(n.String())
		return i, err == nil
	default:
		return 0, false
	}
}

// projectTemplateOverride desaloja al override vivo que ocupe el slot
// (gym, template_key) con un id distinto y delega en projectGeneric. Ver
// el comentario del registro en projectors.
func projectTemplateOverride(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return fmt.Errorf("projector notification_templates: payload not a JSON object: %w", err)
	}
	if key, ok := raw["template_key"].(string); ok && key != "" {
		if err := g.Exec(`
			UPDATE notification_templates
			   SET deleted_at = NOW(), updated_at = NOW(), version = version + 1
			 WHERE gym_id = ? AND template_key = ? AND id <> ? AND deleted_at IS NULL`,
			gymID, key, entityID).Error; err != nil {
			return fmt.Errorf("projector notification_templates: desalojar slot %q: %w", key, err)
		}
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

// projectUser envuelve projectGeneric con una salvaguarda de credenciales.
// IMPORTANTE: esta función sólo corre en el CLOUD, al aplicar un PUSH del
// sidecar (server_store.go, build tag `server`). El sidecar aplica los pulls
// por su cuenta (agent_apply.go, build tag `sidecar`), así que la dirección
// cloud→sidecar del password_hash no pasa por aquí.
func projectUser(g *gorm.DB, table EntityTable, gymID, entityID uuid.UUID, payload []byte) error {
	guarded, err := guardUserCredential(payload)
	if err != nil {
		return fmt.Errorf("projector users: %w", err)
	}
	return projectGeneric(g, table, gymID, entityID, guarded)
}

// guardUserCredential descarta `password_hash` del payload de un push cuando
// dejarlo pasar corrompería el login. Invariante: el cloud es la autoridad
// del password del dashboard del dueño; un push del sidecar NUNCA debe:
//
//   - blanquear un password_hash existente. Cuando el sidecar no tiene
//     `cached_login`, mirrorCloudIdentity siembra la fila local de `users` con
//     password_hash="" (auth_controller_sidecar.go); enqueueUser lo empuja y,
//     sin esta guarda, projector.nullifyEmptyString lo colapsa a NULL en el
//     cloud → el siguiente login del dueño revienta con bcrypt "hashedSecret
//     too short". Este es el bug de "la contraseña deja de funcionar".
//   - reescribir el password_hash de un OWNER. El password del dueño sólo se
//     fija cloud-side (signup / forgot-password / reset). El sidecar nada más
//     tiene un hash cacheado, potencialmente viejo (stale) o de menor costo
//     bcrypt; dejarlo ganar el last-write-wins degrada o revierte la credencial.
//
// projectGeneric omite del UPSERT las columnas ausentes, así que al quitar
// password_hash el `ON CONFLICT` preserva el hash que ya tiene el cloud (y en
// un INSERT de primera vez la columna cae a su default NULL — válido post-019).
//
// Para OPERADORES (login offline/PIN; no usan el dashboard cloud) sí dejamos
// pasar un hash NO vacío: el reset legacy de operador desde recepción (UC-009,
// cableado en cmd/sidecar) debe propagar al cloud. `pin_hash` nunca se toca:
// el PIN se asigna en recepción, que es su autoridad.
func guardUserCredential(payload []byte) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("payload not a JSON object: %w", err)
	}
	ph, present := raw["password_hash"]
	if !present {
		return payload, nil
	}
	hash, _ := ph.(string)
	// Fail-closed: el ÚNICO caso en que dejamos que un push escriba
	// password_hash es un reset legacy de operador (rol "operator" + hash no
	// vacío; UC-009 desde recepción). Todo lo demás —owner, hash vacío, rol
	// ausente, desconocido o con otra capitalización— se descarta y el cloud
	// preserva su hash. Así la defensa no depende del CHECK de rol upstream ni
	// se rompe si aparece un nuevo writer o el backfill público Project().
	isOperator := false
	if r, ok := raw["role"].(string); ok {
		isOperator = strings.EqualFold(strings.TrimSpace(r), "operator")
	}
	if hash == "" || !isOperator {
		delete(raw, "password_hash")
		return json.Marshal(raw)
	}
	return payload, nil
}

// projectMembership envuelve projectGeneric con un pre-step que libera el
// slot del partial unique index `uq_memberships_member_active` cuando la
// fila entrante reclama el slot (status active|pending_payment, no
// soft-deleted). Cualquier otra fila vigente del mismo socio se baja a
// 'replaced' — misma semántica que la renovación del dominio.
func projectMembership(
	g *gorm.DB,
	table EntityTable,
	gymID, entityID uuid.UUID,
	payload []byte,
) error {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return fmt.Errorf("projector memberships: payload not a JSON object: %w", err)
	}
	status, _ := raw["status"].(string)
	claimsSlot := status == "active" || status == "pending_payment"
	// deleted_at nil/ausente significa "fila viva". Si viene set, la
	// proyección la marca borrada — no toca el slot de nadie más.
	deleted := false
	if v, ok := raw["deleted_at"]; ok && v != nil {
		// Cualquier valor no-nulo (string ISO, número epoch) indica delete.
		if s, isStr := v.(string); !isStr || s != "" {
			deleted = true
		}
	}
	memberIDStr, _ := raw["member_id"].(string)
	if claimsSlot && !deleted && memberIDStr != "" {
		memberID, err := uuid.Parse(memberIDStr)
		if err == nil {
			if err := vacateActiveMembershipSlot(g, gymID, memberID, entityID); err != nil {
				return fmt.Errorf("memberships pre-vacate active slot: %w", err)
			}
		}
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

// vacateActiveMembershipSlot baja a 'replaced' cualquier fila de memberships
// que esté actualmente ocupando el slot del partial unique index para
// (gymID, memberID) — excepto la que está por ser proyectada (skipID).
// Espeja la operación en sync_entities (payload + version + server_updated_at)
// para que el próximo pull del sidecar refleje el demote y mantenga
// coherente el server reloj (ADR-001 §3.1). Si no había conflicto, los
// UPDATE no afectan filas y el upsert principal sigue sin cambio.
func vacateActiveMembershipSlot(
	g *gorm.DB,
	gymID, memberID, skipID uuid.UUID,
) error {
	type demoted struct {
		ID      uuid.UUID
		Version int
	}
	var rows []demoted
	// Demote + RETURNING para conocer las filas afectadas y su nueva
	// version (la usamos para sync_entities). updated_at se reformulea
	// con NOW() de cloud — server reloj manda.
	const demoteSQL = `
		UPDATE memberships
		   SET status = 'replaced',
		       updated_at = NOW(),
		       version = version + 1
		 WHERE gym_id = ?
		   AND member_id = ?
		   AND id <> ?
		   AND status IN ('active','pending_payment')
		   AND deleted_at IS NULL
		RETURNING id, version`
	if err := g.Raw(demoteSQL, gymID, memberID, skipID).Scan(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	// Para cada fila demote, sincronizar sync_entities: status, version,
	// updated_at en payload + version + server_updated_at en la columna.
	// El payload usa epoch_ms (mismo formato que el sidecar emite) para
	// que ApplyPullChange en el sidecar no se confunda con el tipo.
	const syncSQL = `
		UPDATE sync_entities
		   SET payload = payload || jsonb_build_object(
		                     'status', 'replaced',
		                     'version', ?::int,
		                     'updated_at', (EXTRACT(EPOCH FROM NOW()) * 1000)::bigint
		                 ),
		       version = ?,
		       server_updated_at = NOW()
		 WHERE gym_id = ?
		   AND entity_type = 'memberships'
		   AND entity_id = ?`
	for _, r := range rows {
		if err := g.Exec(syncSQL, r.Version, r.Version, gymID, r.ID).Error; err != nil {
			return fmt.Errorf("sync_entities demote mirror for %s: %w", r.ID, err)
		}
	}
	return nil
}

// projectMember envuelve projectGeneric con la reconciliación del número de
// socio (ADR-010 §2.3). Si la fila entrante trae un member_number que otro
// socio vivo del mismo gym ya ocupa (violación del índice único
// `uq_members_gym_number`), el que llegó después pierde: se le reasigna un
// número nuevo (max+1 del gym), se proyecta con ese número, se espeja el
// cambio a sync_entities (para que el sidecar adopte el número corregido en
// su próximo pull y deje de reenviar el viejo) y se re-encola su banner de
// bienvenida (ADR-009) en notification_queue para que el cloud lo despache.
//
// La mayoría de gyms son single-device → esto casi nunca se dispara; multi-
// device real implica cadena con internet → asignación efectivamente online.
func projectMember(
	g *gorm.DB,
	table EntityTable,
	gymID, entityID uuid.UUID,
	payload []byte,
) error {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return fmt.Errorf("projector members: payload not a JSON object: %w", err)
	}
	number, hasNumber := memberNumberFromPayload(raw["member_number"])
	deleted := payloadMarksDeleted(raw["deleted_at"])

	reassigned := false
	if hasNumber && !deleted {
		var conflicts int64
		if err := g.Raw(
			`SELECT COUNT(1) FROM members
			   WHERE gym_id = ? AND member_number = ? AND deleted_at IS NULL AND id <> ?`,
			gymID, number, entityID,
		).Scan(&conflicts).Error; err != nil {
			return fmt.Errorf("members reconcile collision check: %w", err)
		}
		if conflicts > 0 {
			newNum, err := nextFreeMemberNumber(g, gymID)
			if err != nil {
				return fmt.Errorf("members reconcile next number: %w", err)
			}
			raw["member_number"] = newNum
			number = newNum
			reassigned = true
			newPayload, err := json.Marshal(raw)
			if err != nil {
				return fmt.Errorf("members reconcile re-marshal: %w", err)
			}
			payload = newPayload
		}
	}

	if err := projectGeneric(g, table, gymID, entityID, payload); err != nil {
		return err
	}

	if reassigned {
		// Espejar a sync_entities (mismo patrón que vacateActiveMembershipSlot):
		// bump version + payload corregido + server reloj, para que el pull del
		// sidecar adopte el número nuevo y converja (sin esto reenviaría el
		// viejo en cada sync y dispararía re-notify en loop).
		if err := g.Exec(
			`UPDATE sync_entities
			    SET payload = payload || jsonb_build_object(
			                      'member_number', ?::int,
			                      'version', (version + 1),
			                      'updated_at', (EXTRACT(EPOCH FROM NOW()) * 1000)::bigint
			                  ),
			        version = version + 1,
			        server_updated_at = NOW()
			  WHERE gym_id = ? AND entity_type = 'members' AND entity_id = ?`,
			number, gymID, entityID,
		).Error; err != nil {
			return fmt.Errorf("members reconcile sync_entities mirror: %w", err)
		}
		if err := enqueueWelcomeRenotify(g, gymID, entityID, number); err != nil {
			return fmt.Errorf("members reconcile re-notify: %w", err)
		}
	}
	return nil
}

// memberNumberFromPayload extrae el número de socio del payload (JSON number,
// json.Number o string). (0, false) si ausente/nulo/no-numérico.
func memberNumberFromPayload(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
	case string:
		if x == "" {
			return 0, false
		}
		if n, err := strconv.Atoi(x); err == nil {
			return n, true
		}
	}
	return 0, false
}

// payloadMarksDeleted replica la heurística de projectMembership: deleted_at
// presente y no-vacío ⇒ fila borrada.
func payloadMarksDeleted(v any) bool {
	if v == nil {
		return false
	}
	if s, isStr := v.(string); isStr {
		return s != ""
	}
	return true
}

// nextFreeMemberNumber returns max(member_number)+1 for the gym (>= 1000).
// max+1 garantiza unicidad en el gym sin necesidad de gap-fill — la
// reconciliación es un evento marginal, no la ruta caliente.
func nextFreeMemberNumber(g *gorm.DB, gymID uuid.UUID) (int, error) {
	var maxNum int
	if err := g.Raw(
		`SELECT COALESCE(MAX(member_number), 999) FROM members
		   WHERE gym_id = ? AND member_number IS NOT NULL AND deleted_at IS NULL`,
		gymID,
	).Scan(&maxNum).Error; err != nil {
		return 0, err
	}
	return maxNum + 1, nil
}

// enqueueWelcomeRenotify inserta una fila member_welcome_number en
// notification_queue para que el cloud despache el banner con el número
// corregido (ADR-010 §2.3 + ADR-009). Escribe SQL directo en lugar de
// importar la notifications BC — shared/sync no debe depender de los módulos.
// Idempotente vía idempotency_key + ON CONFLICT DO NOTHING. Skip silencioso
// si el socio no tiene teléfono (no hay a quién notificar).
func enqueueWelcomeRenotify(g *gorm.DB, gymID, memberID uuid.UUID, number int) error {
	var m struct {
		FullName string
		Phone    string
	}
	if err := g.Raw(
		`SELECT full_name, phone FROM members WHERE id = ? AND gym_id = ?`,
		memberID, gymID,
	).Scan(&m).Error; err != nil {
		return err
	}
	// Normaliza a E.164 antes de encolar (defensa): garantiza que
	// recipient_address del notification_queue quede canónico aunque la fila
	// del socio tenga un valor legacy con espacios/sin código de país.
	phone := phonepkg.Normalize(m.Phone)
	if phone == "" {
		return nil
	}
	var gymName string
	if err := g.Raw(`SELECT COALESCE(name, '') FROM gyms WHERE id = ?`, gymID).Scan(&gymName).Error; err != nil {
		return err
	}

	payload, err := json.Marshal(map[string]string{
		"member_first_name": firstNameOf(m.FullName),
		"gym_name":          gymName,
		"member_number":     strconv.Itoa(number),
	})
	if err != nil {
		return err
	}
	idempKey := fmt.Sprintf("welcome_number:%s:%d", memberID.String(), number)

	return g.Exec(
		`INSERT INTO notification_queue
		    (id, gym_id, version, created_at, updated_at, channel, template_key,
		     recipient_type, recipient_id, recipient_address, payload, status,
		     retry_count, scheduled_for, idempotency_key)
		 VALUES (?, ?, 1, NOW(), NOW(), 'whatsapp', 'member_welcome_number',
		         'member', ?, ?, ?::jsonb, 'pending', 0, NOW(), ?)
		 ON CONFLICT DO NOTHING`,
		uuid.New(), gymID, memberID, phone, string(payload), idempKey,
	).Error
}

// firstNameOf returns the first whitespace-delimited token of a full name
// (mirrors notifications/app.firstName without importing the package).
func firstNameOf(full string) string {
	fields := strings.Fields(full)
	if len(fields) == 0 {
		return full
	}
	return fields[0]
}

// project dispatches to the registered projector for entityType. Returns a
// distinguished error when no projector is wired so the push handler can
// surface it as 500 missing_projector instead of pretending success.
func project(g *gorm.DB, entityType string, gymID, entityID uuid.UUID, payload []byte) error {
	p, ok := projectors[entityType]
	if !ok {
		return fmt.Errorf("missing_projector: no projector registered for entity_type=%q", entityType)
	}
	return p(g, gymID, entityID, payload)
}

// Project is the public wrapper around project() for one-off backfill
// scripts that need to replay sync_entities rows into the domain tables.
// Production push uses the unexported version inline.
func Project(g *gorm.DB, entityType string, gymID, entityID uuid.UUID, payload []byte) error {
	return project(g, entityType, gymID, entityID, payload)
}

// projectGeneric writes the payload into table.Table via INSERT … ON
// CONFLICT (id) DO UPDATE SET … using the column list declared in
// SyncedTables. Only columns present in the payload are emitted, which
// keeps schema defaults intact for new rows and preserves existing values
// for unmentioned columns on conflict (EXCLUDED is symmetric with the
// INSERT column list).
//
// Per-column type adjustments:
//   - JSONB columns get `?::jsonb` and the value is re-marshalled to JSON.
//   - BYTEA columns get base64-decoded into []byte.
//   - TIMESTAMPTZ columns receiving epoch-ms numbers get converted to
//     time.Time (Postgres rejects raw floats for timestamptz).
//   - Everything else rides pgx's native conversions.
func projectGeneric(
	g *gorm.DB,
	table EntityTable,
	gymID, entityID uuid.UUID,
	payload []byte,
) error {
	q, args, err := buildProjectorUpsert(table, gymID, entityID, payload)
	if err != nil {
		return err
	}
	return g.Exec(q, args...).Error
}

// buildProjectorUpsert produces the SQL + args for projectGeneric without
// executing it. Split out so unit tests can verify SQL shape without a
// running Postgres, and so the executor stays a one-liner.
func buildProjectorUpsert(
	table EntityTable,
	gymID, entityID uuid.UUID,
	payload []byte,
) (string, []any, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return "", nil, fmt.Errorf("projector %s: payload not a JSON object: %w", table.Type, err)
	}

	// Apply payload-key aliases (e.g. deleted_at_ms → deleted_at).
	if aliases := payloadKeyAliases[table.Table]; len(aliases) > 0 {
		for src, dst := range aliases {
			if v, ok := raw[src]; ok {
				raw[dst] = v
				delete(raw, src)
			}
		}
	}

	// Force id and gym_id from the upsert context (defense in depth — the
	// caller already validated payload.gym_id matches the JWT, but the
	// projector is the last writer so we re-pin both to avoid any future
	// bypass). Composite-key tables have no `id` column, so the assignment
	// is a no-op for them: the loop below only emits columns that appear in
	// table.Columns.
	raw["id"] = entityID.String()
	raw["gym_id"] = gymID.String()

	jsonCols := jsonbColumns[table.Table]
	byteCols := byteaColumns[table.Table]

	cols := make([]string, 0, len(table.Columns))
	placeholders := make([]string, 0, len(table.Columns))
	args := make([]any, 0, len(table.Columns))

	for _, c := range table.Columns {
		v, present := raw[c]
		if !present {
			continue
		}
		var (
			ph  = "?"
			arg any
		)
		switch {
		case jsonCols[c]:
			converted, err := encodeJSONB(v)
			if err != nil {
				return "", nil, fmt.Errorf("projector %s column %s: %w", table.Type, c, err)
			}
			arg = converted
			if converted != nil {
				ph = "?::jsonb"
			}
		case byteCols[c]:
			b, err := decodeBytea(v)
			if err != nil {
				return "", nil, fmt.Errorf("projector %s column %s: %w", table.Type, c, err)
			}
			arg = b
		case isTimestampColumn(table.Table, c):
			arg = coerceTimestamp(v)
		case notNullStringColumns[table.Table][c]:
			// NOT NULL column whose dominio acepta "" como ausencia válida —
			// preservamos el string vacío en lugar de colapsarlo a NULL (que
			// la constraint rechazaría). Ver notNullStringColumns arriba.
			arg = v
		default:
			arg = nullifyEmptyString(v)
		}
		cols = append(cols, c)
		placeholders = append(placeholders, ph)
		args = append(args, arg)
	}

	if len(cols) == 0 {
		return "", nil, fmt.Errorf("projector %s: empty payload", table.Type)
	}

	conflictCols := []string{"id"}
	if len(table.CompositeKey) > 0 {
		conflictCols = table.CompositeKey
	}
	conflictSet := make(map[string]struct{}, len(conflictCols))
	for _, c := range conflictCols {
		conflictSet[c] = struct{}{}
	}

	setClauses := make([]string, 0, len(cols))
	for _, c := range cols {
		// Don't update the conflict-key columns themselves on update —
		// they're the dedupe identity.
		if _, isKey := conflictSet[c]; isKey {
			continue
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = EXCLUDED.%s", c, c))
	}

	q := fmt.Sprintf(
		`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s`,
		table.Table,
		strings.Join(cols, ", "),
		strings.Join(placeholders, ", "),
		strings.Join(conflictCols, ", "),
		strings.Join(setClauses, ", "),
	)
	return q, args, nil
}

// encodeJSONB normalises any-typed payload values into the JSON text that
// pgx will hand to a `?::jsonb` cast. nil round-trips as SQL NULL; pre-
// stringified JSON passes through; anything else is re-marshalled.
func encodeJSONB(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return x, nil
	case json.RawMessage:
		return string(x), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("jsonb marshal: %w", err)
		}
		return string(b), nil
	}
}

// decodeBytea unwraps a base64 payload value into raw bytes for BYTEA
// columns. The sidecar always sends BYTEA fields base64-encoded
// (sync_queue.payload is TEXT and JSON cannot carry binary).
func decodeBytea(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if x == "" {
			return nil, nil
		}
		return base64.StdEncoding.DecodeString(x)
	default:
		return nil, fmt.Errorf("bytea expected string, got %T", v)
	}
}

// isTimestampColumn returns true for columns whose Postgres type is
// timestamptz and whose payload value may arrive as an epoch-ms number.
//
// La mayoría de columnas timestamp del schema terminan en `_at`, así que
// esa es la heurística rápida. Para las que no (medición t0/t1 de retos,
// scheduled_for de notification_queue), el override por tabla en
// timestamptzColumns las atrapa. Las columnas tipo `_date` / `birthdate`
// llegan como strings ISO y Postgres las parsea sin coerción.
func isTimestampColumn(table, name string) bool {
	if strings.HasSuffix(name, "_at") {
		return true
	}
	if cols := timestamptzColumns[table]; cols != nil {
		return cols[name]
	}
	return false
}

// nullifyEmptyString collapses sidecar's `""` defaults into SQL NULL.
//
// The sidecar's enqueue helpers default optional `*string` fields to `""`
// when nil, which round-trips through JSON as the empty string. Postgres
// distinguishes the empty string from NULL — and several nullable
// columns have CHECK constraints (e.g. `chk_membership_types_frequency`)
// that explicitly reject empty strings. Converting empty strings to nil
// here unifies the two
// representations on the projector boundary, so neither side has to
// migrate. NOT NULL columns whose payload is genuinely `""` would still
// fail the constraint after this — but the existing enqueue code never
// sends `""` for any NOT NULL string column.
func nullifyEmptyString(v any) any {
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	return v
}

// coerceTimestamp turns an epoch-ms float (the sidecar's wire format) into
// a time.Time. Anything else (string, nil, time.Time) passes through —
// Postgres parses ISO strings into timestamptz natively.
func coerceTimestamp(v any) any {
	switch x := v.(type) {
	case float64:
		return time.UnixMilli(int64(x)).UTC()
	case int64:
		return time.UnixMilli(x).UTC()
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return time.UnixMilli(n).UTC()
		}
		return v
	default:
		return v
	}
}
