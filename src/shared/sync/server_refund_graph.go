//go:build server

package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/google/uuid"
	"gorm.io/gorm"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

const (
	atomicRefundSetting = "tinta.atomic_refund_graph"
)

type refundPushUnit struct {
	indexes  []int
	graphIDs []string
}

// groupRefundPushItems returns connected components. A mutable root Payment
// may participate in several offline refunds, so graph ids form a small
// hypergraph rather than a simple one-refund/one-batch mapping.
func groupRefundPushItems(batch []PushItem) []refundPushUnit {
	sets := make([]map[string]struct{}, len(batch))
	for i := range batch {
		sets[i] = make(map[string]struct{})
		for _, graphID := range refundGraphIDs(batch[i].Payload) {
			sets[i][graphID] = struct{}{}
		}
	}

	// Compatibility for sidecars created before explicit graph metadata: when
	// all rows happen to share a batch, infer the same component from the
	// immutable Refund links. A split legacy batch remains fail-closed.
	for i, item := range batch {
		if item.EntityType != "refunds" {
			continue
		}
		graphID := item.EntityID
		sets[i][graphID] = struct{}{}
		raw := decodePushPayload(item.Payload)
		rootID, _ := raw["root_payment_id"].(string)
		refundPaymentID, _ := raw["refund_payment_id"].(string)
		sourceID := ""
		for j, candidate := range batch {
			if candidate.EntityType == "payments" && candidate.EntityID == refundPaymentID {
				paymentRaw := decodePushPayload(candidate.Payload)
				sourceID, _ = paymentRaw["parent_payment_id"].(string)
				sets[j][graphID] = struct{}{}
			}
		}
		for j, candidate := range batch {
			switch candidate.EntityType {
			case "payments":
				if candidate.EntityID == rootID || candidate.EntityID == sourceID {
					sets[j][graphID] = struct{}{}
				}
			case "refund_items":
				candidateRaw := decodePushPayload(candidate.Payload)
				if refundID, _ := candidateRaw["refund_id"].(string); refundID == graphID {
					sets[j][graphID] = struct{}{}
				}
			}
		}
	}
	for i, item := range batch {
		if item.EntityType != "refund_items" {
			continue
		}
		if refundID, _ := decodePushPayload(item.Payload)["refund_id"].(string); refundID != "" {
			sets[i][refundID] = struct{}{}
		}
	}

	parent := make([]int, len(batch))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	owners := make(map[string]int)
	for i, graphIDs := range sets {
		for graphID := range graphIDs {
			if first, ok := owners[graphID]; ok {
				union(first, i)
			} else {
				owners[graphID] = i
			}
		}
	}

	byRoot := make(map[int]*refundPushUnit)
	for i := range batch {
		root := find(i)
		unit := byRoot[root]
		if unit == nil {
			unit = &refundPushUnit{}
			byRoot[root] = unit
		}
		unit.indexes = append(unit.indexes, i)
	}
	for root, unit := range byRoot {
		graphs := make(map[string]struct{})
		for _, index := range unit.indexes {
			for graphID := range sets[index] {
				graphs[graphID] = struct{}{}
			}
		}
		unit.graphIDs = make([]string, 0, len(graphs))
		for graphID := range graphs {
			unit.graphIDs = append(unit.graphIDs, graphID)
		}
		sort.Strings(unit.graphIDs)
		byRoot[root] = unit
	}
	units := make([]refundPushUnit, 0, len(byRoot))
	for _, unit := range byRoot {
		units = append(units, *unit)
	}
	sort.Slice(units, func(i, j int) bool { return units[i].indexes[0] < units[j].indexes[0] })
	return units
}

type pushStatusError struct {
	status string
	msg    string
}

func (e *pushStatusError) Error() string { return e.msg }

func (h *Handler) processRefundGraph(
	ctx context.Context,
	gymID, clientID uuid.UUID,
	batch []PushItem,
	unit refundPushUnit,
) map[int]PushItemResult {
	results := make(map[int]PushItemResult, len(unit.indexes))
	for _, index := range unit.indexes {
		item := batch[index]
		results[index] = PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID}
		if invalid := validatePushItem(item); invalid != nil {
			return rejectRefundUnit(batch, unit, invalid.Status, invalid.Error)
		}
		if item.EntityType != "payments" && item.EntityType != "refunds" && item.EntityType != "refund_items" &&
			item.EntityType != "sales" && item.EntityType != "sale_items" && item.EntityType != "sale_corrections" {
			return rejectRefundUnit(batch, unit, StatusRejectedFinancialConflict,
				"el grafo de devolución contiene una entidad que no le pertenece")
		}
	}
	graphIDs := make([]uuid.UUID, 0, len(unit.graphIDs))
	for _, value := range unit.graphIDs {
		graphID, err := uuid.Parse(value)
		if err != nil {
			return rejectRefundUnit(batch, unit, StatusRejectedFinancialConflict,
				"el grafo de devolución tiene un identificador inválido")
		}
		graphIDs = append(graphIDs, graphID)
	}
	if len(graphIDs) == 0 {
		return rejectRefundUnit(batch, unit, StatusRejectedFinancialConflict,
			"el grafo de devolución no identifica su agregado")
	}

	ordered := append([]int(nil), unit.indexes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return refundPushRank(batch[ordered[i]]) < refundPushRank(batch[ordered[j]])
	})
	err := h.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		g := gormTx(tx).WithContext(ctx)
		if err := g.Exec(`SELECT set_config(?, '1', true)`, atomicRefundSetting).Error; err != nil {
			return fmt.Errorf("activar transacción de devolución atómica: %w", err)
		}
		if err := validateRefundGraphInput(g, gymID, graphIDs, batch, unit); err != nil {
			return err
		}
		for _, index := range ordered {
			item := batch[index]
			out := results[index]
			if err := h.applyOneInTx(ctx, tx, gymID, clientID, item, &out); err != nil {
				return err
			}
			results[index] = out
			switch out.Status {
			case StatusAccepted, StatusConflictClientWins, StatusConflictServerWins:
				// Validated against the canonical aggregate below.
			default:
				message := out.Error
				if message == "" {
					message = "una fila del grafo fue rechazada"
				}
				return &pushStatusError{status: out.Status, msg: message}
			}
		}
		return validateAtomicRefundGraphs(g, gymID, graphIDs, batch, unit)
	})
	if err == nil {
		return results
	}
	status, message := StatusRejectedInternal, err.Error()
	var statusErr *pushStatusError
	var financialConflict *financialConflictError
	switch {
	case errors.As(err, &statusErr):
		status, message = statusErr.status, statusErr.msg
	case errors.As(err, &financialConflict):
		status, message = StatusRejectedFinancialConflict, financialConflict.Error()
	default:
		if duplicateMessage, duplicate := mapUniqueViolation(err, batch[unit.indexes[0]]); duplicate {
			status, message = StatusRejectedDuplicate, duplicateMessage
		}
	}
	return rejectRefundUnit(batch, unit, status, message)
}

// validateRefundGraphInput proves that the connected component contains only
// rows belonging to the Refund aggregates named by graphIDs. Metadata is a
// transport hint, not authority: without this closure check a tampered client
// could tag an unrelated positive Payment and make it commit alongside an
// otherwise valid refund.
//
// Existing positive Payments are also guarded before any row is projected.
// A normal refund may update the mutable balance_pending snapshot, but it may
// not rewrite the original collection facts. A SaleCorrection in the same
// graph is the only evidence that permits amount/recognition/discount/tombstone
// changes; that correction is validated against its immutable snapshots later
// in the same transaction.
func validateRefundGraphInput(
	g *gorm.DB,
	gymID uuid.UUID,
	graphIDs []uuid.UUID,
	batch []PushItem,
	unit refundPushUnit,
) error {
	graphs := make(map[uuid.UUID]struct{}, len(graphIDs))
	for _, id := range graphIDs {
		graphs[id] = struct{}{}
	}

	refunds := make(map[uuid.UUID]map[string]any, len(graphIDs))
	rootPayments := make(map[uuid.UUID]struct{})
	refundPayments := make(map[uuid.UUID]struct{})
	refundSales := make(map[uuid.UUID]struct{})
	directCorrections := make(map[uuid.UUID]struct{})
	correctionSales := make(map[uuid.UUID]struct{})
	positivePayments := make(map[uuid.UUID]map[string]any)
	negativePayments := make(map[uuid.UUID]map[string]any)
	sourcePayments := make(map[uuid.UUID]struct{})
	sales := make(map[uuid.UUID]map[string]any)
	corrections := make(map[uuid.UUID]map[string]any)

	for _, index := range unit.indexes {
		item := batch[index]
		entityID, err := uuid.Parse(item.EntityID)
		if err != nil || entityID == uuid.Nil {
			return newFinancialConflict("el grafo de devolución contiene un identificador de entidad inválido")
		}
		raw := decodePushPayload(item.Payload)
		if raw == nil || raw["id"] != item.EntityID {
			return newFinancialConflict("una fila del grafo no coincide con su identificador de transporte")
		}
		switch item.EntityType {
		case "refunds":
			if _, duplicate := refunds[entityID]; duplicate {
				return newFinancialConflict("el grafo repite la devolución %s", entityID)
			}
			refunds[entityID] = raw
			rootID, parseErr := projectorUUID(raw, "root_payment_id")
			if parseErr != nil {
				return parseErr
			}
			rootPayments[rootID] = struct{}{}
			if value := projectorOptionalUUID(raw["refund_payment_id"]); value != nil {
				refundPayments[*value] = struct{}{}
			}
			if value := projectorOptionalUUID(raw["sale_id"]); value != nil {
				refundSales[*value] = struct{}{}
			}
			if value := projectorOptionalUUID(raw["correction_id"]); value != nil {
				directCorrections[*value] = struct{}{}
			}
		case "payments":
			concept, _ := raw["concept"].(string)
			if concept == "refund" {
				negativePayments[entityID] = raw
				sourceID, parseErr := projectorUUID(raw, "parent_payment_id")
				if parseErr != nil {
					return parseErr
				}
				sourcePayments[sourceID] = struct{}{}
			} else {
				positivePayments[entityID] = raw
			}
		case "sales":
			sales[entityID] = raw
		case "sale_corrections":
			corrections[entityID] = raw
			saleID, parseErr := projectorUUID(raw, "sale_id")
			if parseErr != nil {
				return parseErr
			}
			correctionSales[saleID] = struct{}{}
		case "sale_items":
			saleID, parseErr := projectorUUID(raw, "sale_id")
			if parseErr != nil {
				return parseErr
			}
			// Sale lines only travel with an unsynced correction snapshot.
			// Ordinary product refunds carry refund_items, never sale_items.
			if _, belongs := correctionSales[saleID]; !belongs {
				// Corrections can appear later in transport order, so verify in a
				// second pass below after every row has been decoded.
				continue
			}
		case "refund_items":
			// Relationship checked below after every Refund has been decoded.
		}
	}

	if len(refunds) != len(graphs) {
		return newFinancialConflict("grafo de devolución incompleto: faltan devoluciones del componente")
	}
	for id := range graphs {
		if _, present := refunds[id]; !present {
			return newFinancialConflict("grafo de devolución incompleto: falta la devolución %s", id)
		}
	}
	for id := range refunds {
		if _, belongs := graphs[id]; !belongs {
			return newFinancialConflict("la devolución %s no pertenece al componente declarado", id)
		}
	}
	for id := range negativePayments {
		if _, referenced := refundPayments[id]; !referenced {
			return newFinancialConflict("el grafo contiene un movimiento de devolución no relacionado")
		}
	}
	for id := range positivePayments {
		_, isRoot := rootPayments[id]
		_, isSource := sourcePayments[id]
		if !isRoot && !isSource {
			return newFinancialConflict("el grafo contiene un cobro positivo no relacionado")
		}
	}
	for id, raw := range corrections {
		saleID, _ := projectorUUID(raw, "sale_id")
		if _, relatedSale := refundSales[saleID]; !relatedSale {
			return newFinancialConflict("la corrección %s no pertenece a la venta devuelta", id)
		}
	}
	for id := range directCorrections {
		if _, inBatch := corrections[id]; inBatch {
			continue
		}
		var present bool
		if err := g.Raw(`SELECT EXISTS(SELECT 1 FROM sale_corrections
			WHERE gym_id=? AND id=? AND deleted_at IS NULL)`, gymID, id).Scan(&present).Error; err != nil {
			return fmt.Errorf("validar corrección referenciada por devolución: %w", err)
		}
		if !present {
			return newFinancialConflict("grafo de devolución incompleto: falta la corrección %s", id)
		}
	}

	correctedPayments := make(map[uuid.UUID]struct{})
	for saleID := range correctionSales {
		var paymentID uuid.UUID
		if raw, inBatch := sales[saleID]; inBatch {
			parsed, parseErr := projectorUUID(raw, "payment_id")
			if parseErr != nil {
				return parseErr
			}
			paymentID = parsed
		} else {
			result := g.Raw(`SELECT payment_id FROM sales WHERE gym_id=? AND id=?`, gymID, saleID).Scan(&paymentID)
			if result.Error != nil {
				return fmt.Errorf("validar venta corregida del grafo: %w", result.Error)
			}
			if result.RowsAffected == 0 || paymentID == uuid.Nil {
				return newFinancialConflict("la venta corregida del grafo no existe en la nube")
			}
		}
		correctedPayments[paymentID] = struct{}{}
	}
	for saleID := range sales {
		if _, related := refundSales[saleID]; !related {
			return newFinancialConflict("el grafo contiene una venta no relacionada")
		}
	}

	for _, index := range unit.indexes {
		item := batch[index]
		raw := decodePushPayload(item.Payload)
		switch item.EntityType {
		case "refund_items":
			refundID, parseErr := projectorUUID(raw, "refund_id")
			if parseErr != nil {
				return parseErr
			}
			if _, belongs := graphs[refundID]; !belongs {
				return newFinancialConflict("el grafo contiene un producto de otra devolución")
			}
		case "sale_items":
			saleID, parseErr := projectorUUID(raw, "sale_id")
			if parseErr != nil {
				return parseErr
			}
			if _, belongs := correctionSales[saleID]; !belongs {
				return newFinancialConflict("el grafo contiene un producto de una venta no corregida")
			}
		}
	}

	for paymentID, raw := range positivePayments {
		_, corrected := correctedPayments[paymentID]
		if err := guardExistingRefundGraphPayment(g, gymID, paymentID, raw, corrected); err != nil {
			return err
		}
	}
	return nil
}

type refundGraphPaymentFacts struct {
	Concept         string     `gorm:"column:concept"`
	ParentID        *uuid.UUID `gorm:"column:parent_id"`
	MemberID        *uuid.UUID `gorm:"column:member_id"`
	MembershipID    *uuid.UUID `gorm:"column:membership_id"`
	OperatorID      uuid.UUID  `gorm:"column:operator_id"`
	CashDrawerID    *uuid.UUID `gorm:"column:cash_drawer_id"`
	Folio           string     `gorm:"column:folio"`
	Method          string     `gorm:"column:payment_method"`
	PaymentDate     string     `gorm:"column:payment_date"`
	AmountCents     int64      `gorm:"column:amount_cents"`
	RecognizedCents int64      `gorm:"column:recognized_cents"`
	DiscountCents   int64      `gorm:"column:discount_cents"`
	Deleted         bool       `gorm:"column:deleted"`
}

func guardExistingRefundGraphPayment(
	g *gorm.DB,
	gymID, paymentID uuid.UUID,
	raw map[string]any,
	corrected bool,
) error {
	var canonical refundGraphPaymentFacts
	result := g.Raw(`SELECT concept,parent_payment_id AS parent_id,member_id,membership_id,operator_id,
		cash_drawer_id,folio,payment_method,payment_date::text AS payment_date,
		ROUND(amount*100)::bigint AS amount_cents,
		ROUND(recognized_amount*100)::bigint AS recognized_cents,
		ROUND(discount_amount*100)::bigint AS discount_cents,
		(deleted_at IS NOT NULL) AS deleted
		FROM payments WHERE gym_id=? AND id=? FOR UPDATE`, gymID, paymentID).Scan(&canonical)
	if result.Error != nil {
		return fmt.Errorf("validar cobro existente del grafo: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil
	}
	incoming, err := refundGraphPaymentFactsFromPayload(raw)
	if err != nil {
		return err
	}
	identityMatches := canonical.Concept == incoming.Concept &&
		sameProjectorOptionalUUID(canonical.ParentID, incoming.ParentID) &&
		sameProjectorOptionalUUID(canonical.MemberID, incoming.MemberID) &&
		sameProjectorOptionalUUID(canonical.MembershipID, incoming.MembershipID) &&
		canonical.OperatorID == incoming.OperatorID &&
		sameProjectorOptionalUUID(canonical.CashDrawerID, incoming.CashDrawerID) &&
		canonical.Folio == incoming.Folio && canonical.Method == incoming.Method &&
		canonical.PaymentDate == incoming.PaymentDate
	if !identityMatches {
		return newFinancialConflict("el cobro %s no puede cambiar sus datos originales dentro de una devolución", paymentID)
	}
	if !corrected && (canonical.AmountCents != incoming.AmountCents ||
		canonical.RecognizedCents != incoming.RecognizedCents ||
		canonical.DiscountCents != incoming.DiscountCents || canonical.Deleted != incoming.Deleted) {
		return newFinancialConflict("el cobro %s no puede cambiar su efecto financiero dentro de una devolución", paymentID)
	}
	return nil
}

func refundGraphPaymentFactsFromPayload(raw map[string]any) (refundGraphPaymentFacts, error) {
	concept, conceptOK := raw["concept"].(string)
	folio, folioOK := raw["folio"].(string)
	method, methodOK := raw["payment_method"].(string)
	paymentDate, dateOK := raw["payment_date"].(string)
	operatorID, operatorErr := projectorUUID(raw, "operator_id")
	amount, amountErr := projectorSignedMoneyCents(raw, "amount")
	recognized, recognizedErr := projectorMoneyCents(raw, "recognized_amount")
	discount, discountErr := projectorMoneyCents(raw, "discount_amount")
	parentID, parentOK := strictOptionalProjectorUUID(raw["parent_payment_id"])
	memberID, memberOK := strictOptionalProjectorUUID(raw["member_id"])
	membershipID, membershipOK := strictOptionalProjectorUUID(raw["membership_id"])
	cashDrawerID, drawerOK := strictOptionalProjectorUUID(raw["cash_drawer_id"])
	if !conceptOK || concept == "" || !folioOK || !methodOK || !dateOK || operatorErr != nil ||
		amountErr != nil || recognizedErr != nil || discountErr != nil ||
		!parentOK || !memberOK || !membershipOK || !drawerOK {
		return refundGraphPaymentFacts{}, newFinancialConflict("el cobro del grafo contiene datos financieros inválidos")
	}
	return refundGraphPaymentFacts{Concept: concept, ParentID: parentID, MemberID: memberID,
		MembershipID: membershipID, OperatorID: operatorID, CashDrawerID: cashDrawerID,
		Folio: folio, Method: method, PaymentDate: paymentDate, AmountCents: amount,
		RecognizedCents: recognized, DiscountCents: discount, Deleted: raw["deleted_at"] != nil}, nil
}

func strictOptionalProjectorUUID(value any) (*uuid.UUID, bool) {
	if value == nil {
		return nil, true
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return nil, false
	}
	id, err := uuid.Parse(text)
	if err != nil || id == uuid.Nil {
		return nil, false
	}
	return &id, true
}

func rejectRefundUnit(batch []PushItem, unit refundPushUnit, status, message string) map[int]PushItemResult {
	out := make(map[int]PushItemResult, len(unit.indexes))
	for _, index := range unit.indexes {
		out[index] = PushItemResult{QueueID: batch[index].QueueID, EntityID: batch[index].EntityID,
			Status: status, Error: message}
	}
	return out
}

func refundPushRank(item PushItem) int {
	switch item.EntityType {
	case "payments":
		if concept, _ := decodePushPayload(item.Payload)["concept"].(string); concept == "refund" {
			return 4
		}
		return 0
	case "sales":
		return 1
	case "sale_items":
		return 2
	case "sale_corrections":
		return 3
	case "refunds":
		return 5
	case "refund_items":
		return 6
	default:
		return 7
	}
}

type canonicalRefundGraph struct {
	ID               uuid.UUID       `gorm:"column:id"`
	RootPaymentID    uuid.UUID       `gorm:"column:root_payment_id"`
	RefundPaymentID  *uuid.UUID      `gorm:"column:refund_payment_id"`
	SaleID           *uuid.UUID      `gorm:"column:sale_id"`
	CorrectionID     *uuid.UUID      `gorm:"column:correction_id"`
	AmountCents      int64           `gorm:"column:amount_cents"`
	CancelledCents   int64           `gorm:"column:cancelled_cents"`
	Method           string          `gorm:"column:method"`
	Kind             string          `gorm:"column:kind"`
	LegacyIncomplete bool            `gorm:"column:legacy_incomplete"`
	IdempotencyKey   string          `gorm:"column:idempotency_key"`
	Fingerprint      string          `gorm:"column:idempotency_fingerprint"`
	JournalPayload   json.RawMessage `gorm:"column:journal_payload"`
}

type canonicalRefundItem struct {
	ID             uuid.UUID `gorm:"column:id"`
	RefundID       uuid.UUID `gorm:"column:refund_id"`
	SaleItemID     uuid.UUID `gorm:"column:sale_item_id"`
	SaleID         uuid.UUID `gorm:"column:sale_id"`
	Quantity       int       `gorm:"column:quantity"`
	AmountCents    int64     `gorm:"column:amount_cents"`
	Disposition    string    `gorm:"column:disposition"`
	SaleQuantity   int       `gorm:"column:sale_quantity"`
	LineTotalCents int64     `gorm:"column:line_total_cents"`
}

func validateAtomicRefundGraphs(
	g *gorm.DB,
	gymID uuid.UUID,
	graphIDs []uuid.UUID,
	batch []PushItem,
	unit refundPushUnit,
) error {
	inputRefunds := make(map[uuid.UUID]map[string]any)
	inputItems := make(map[uuid.UUID]map[string]any)
	for _, index := range unit.indexes {
		item := batch[index]
		raw := decodePushPayload(item.Payload)
		switch item.EntityType {
		case "refunds":
			id, err := uuid.Parse(item.EntityID)
			if err != nil {
				return newFinancialConflict("el grafo contiene una devolución inválida")
			}
			inputRefunds[id] = raw
		case "refund_items":
			id, err := uuid.Parse(item.EntityID)
			if err != nil {
				return newFinancialConflict("el grafo contiene un producto devuelto inválido")
			}
			inputItems[id] = raw
		}
	}

	for _, graphID := range graphIDs {
		var refund canonicalRefundGraph
		result := g.Raw(`SELECT r.id,r.root_payment_id,r.refund_payment_id,r.sale_id,r.correction_id,
			ROUND(r.amount*100)::bigint AS amount_cents,
			ROUND(r.balance_cancelled*100)::bigint AS cancelled_cents,
			COALESCE(r.method,'') AS method,r.kind,r.legacy_incomplete,
			r.idempotency_key,r.idempotency_fingerprint,se.payload AS journal_payload
			FROM refunds r
			LEFT JOIN sync_entities se ON se.gym_id=r.gym_id AND se.entity_type='refunds' AND se.entity_id=r.id
			WHERE r.gym_id=? AND r.id=? AND r.deleted_at IS NULL FOR UPDATE OF r`, gymID, graphID).Scan(&refund)
		if result.Error != nil {
			return fmt.Errorf("validar grafo de devolución: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return newFinancialConflict("grafo de devolución incompleto: falta la devolución %s", graphID)
		}
		payload := inputRefunds[graphID]
		if payload == nil && len(refund.JournalPayload) > 0 {
			payload = decodePushPayload(refund.JournalPayload)
		}
		if payload != nil && !canonicalRefundMatchesPayload(refund, payload) {
			return newFinancialConflict("la devolución %s no coincide con la evidencia del grafo", graphID)
		}

		var items []canonicalRefundItem
		if err := g.Raw(`SELECT ri.id,ri.refund_id,ri.sale_item_id,si.sale_id,
			ri.quantity,ROUND(ri.amount*100)::bigint AS amount_cents,ri.disposition,
			si.quantity AS sale_quantity,ROUND(si.line_total*100)::bigint AS line_total_cents
			FROM refund_items ri
			JOIN sale_items si ON si.gym_id=ri.gym_id AND si.id=ri.sale_item_id AND si.deleted_at IS NULL
			WHERE ri.gym_id=? AND ri.refund_id=? AND ri.deleted_at IS NULL
			ORDER BY ri.id FOR UPDATE OF ri,si`, gymID, graphID).Scan(&items).Error; err != nil {
			return fmt.Errorf("validar productos de la devolución: %w", err)
		}
		expected, present, valid := refundExpectedItemCount(payload)
		if present && !valid {
			return newFinancialConflict("el grafo de devolución declara una cantidad de productos inválida")
		}
		if _, tagged := payload[refundGraphIDsKey]; tagged && !present {
			return newFinancialConflict("grafo de devolución incompleto: falta la cantidad esperada de productos")
		}
		if present && expected != len(items) {
			return newFinancialConflict(
				"grafo de devolución incompleto: llegaron %d de %d productos", len(items), expected,
			)
		}
		canonicalByID := make(map[uuid.UUID]canonicalRefundItem, len(items))
		var itemTotal int64
		for _, item := range items {
			canonicalByID[item.ID] = item
			itemTotal += item.AmountCents
		}
		for itemID, raw := range inputItems {
			refundID := projectorOptionalUUID(raw["refund_id"])
			if refundID == nil || *refundID != graphID {
				continue
			}
			canonical, exists := canonicalByID[itemID]
			if !exists || !canonicalRefundItemMatchesPayload(canonical, raw) {
				return newFinancialConflict("un producto devuelto no coincide con la evidencia del grafo")
			}
		}

		economicCents := refund.AmountCents + refund.CancelledCents
		switch {
		case refund.Kind == "overcollection_settlement":
			if len(items) != 0 {
				return newFinancialConflict("una liquidación de cobro excedente no puede incluir productos")
			}
		case refund.SaleID == nil:
			if len(items) != 0 {
				return newFinancialConflict("una devolución sin venta no puede incluir productos")
			}
		case refund.LegacyIncomplete && len(items) == 0:
			// Historical imported evidence may predate itemised refunds. New
			// sidecars never emit this flag.
		case len(items) == 0 || itemTotal != economicCents:
			return newFinancialConflict(
				"grafo de devolución incompleto: el detalle suma %s y el efecto total es %s",
				formatProjectorCents(itemTotal), formatProjectorCents(economicCents),
			)
		default:
			for _, item := range items {
				if item.SaleID != *refund.SaleID {
					return newFinancialConflict("un producto devuelto no pertenece a la venta indicada")
				}
			}
			if err := validateSaleRefundAllocation(g, gymID, *refund.SaleID); err != nil {
				return err
			}
		}
	}
	return nil
}

func canonicalRefundMatchesPayload(refund canonicalRefundGraph, raw map[string]any) bool {
	rootID, rootErr := projectorUUID(raw, "root_payment_id")
	amount, amountErr := projectorMoneyCents(raw, "amount")
	cancelled, cancelledErr := projectorMoneyCents(raw, "balance_cancelled")
	method, _ := raw["method"].(string)
	kind, _ := raw["kind"].(string)
	if kind == "" {
		kind = "revenue_refund"
	}
	key, _ := raw["idempotency_key"].(string)
	fingerprint, _ := raw["idempotency_fingerprint"].(string)
	return rootErr == nil && amountErr == nil && cancelledErr == nil &&
		rootID == refund.RootPaymentID && amount == refund.AmountCents && cancelled == refund.CancelledCents &&
		method == refund.Method && kind == refund.Kind && key == refund.IdempotencyKey && fingerprint == refund.Fingerprint &&
		sameProjectorOptionalUUID(refund.RefundPaymentID, projectorOptionalUUID(raw["refund_payment_id"])) &&
		sameProjectorOptionalUUID(refund.SaleID, projectorOptionalUUID(raw["sale_id"])) &&
		sameProjectorOptionalUUID(refund.CorrectionID, projectorOptionalUUID(raw["correction_id"]))
}

func canonicalRefundItemMatchesPayload(item canonicalRefundItem, raw map[string]any) bool {
	refundID, refundErr := projectorUUID(raw, "refund_id")
	saleItemID, saleItemErr := projectorUUID(raw, "sale_item_id")
	quantity, quantityOK := numericInt(raw["quantity"])
	amount, amountErr := projectorMoneyCents(raw, "amount")
	disposition, _ := raw["disposition"].(string)
	return refundErr == nil && saleItemErr == nil && quantityOK && amountErr == nil &&
		refundID == item.RefundID && saleItemID == item.SaleItemID && quantity == item.Quantity &&
		amount == item.AmountCents && disposition == item.Disposition && raw["deleted_at"] == nil
}

func refundExpectedItemCount(raw map[string]any) (count int, present, valid bool) {
	if raw == nil {
		return 0, false, false
	}
	value, present := raw[refundGraphExpectedItemsKey]
	if !present {
		return 0, false, false
	}
	count, ok := numericInt(value)
	return count, true, ok && count >= 0
}

func validateSaleRefundAllocation(g *gorm.DB, gymID, saleID uuid.UUID) error {
	var sale struct {
		SubtotalCents int64 `gorm:"column:subtotal_cents"`
		TotalCents    int64 `gorm:"column:total_cents"`
	}
	result := g.Raw(`SELECT ROUND(subtotal*100)::bigint AS subtotal_cents,
		ROUND(total*100)::bigint AS total_cents
		FROM sales WHERE gym_id=? AND id=? AND deleted_at IS NULL FOR UPDATE`, gymID, saleID).Scan(&sale)
	if result.Error != nil {
		return fmt.Errorf("validar venta de la devolución: %w", result.Error)
	}
	if result.RowsAffected == 0 || sale.SubtotalCents <= 0 || sale.TotalCents <= 0 {
		return newFinancialConflict("la venta devuelta no existe o no conserva un total válido")
	}
	type line struct {
		ID             uuid.UUID `gorm:"column:id"`
		Quantity       int       `gorm:"column:quantity"`
		LineTotalCents int64     `gorm:"column:line_total_cents"`
		RefundedQty    int       `gorm:"column:refunded_qty"`
		RefundedCents  int64     `gorm:"column:refunded_cents"`
	}
	var lines []line
	if err := g.Raw(`SELECT si.id,si.quantity,ROUND(si.line_total*100)::bigint AS line_total_cents,
		COALESCE((SELECT SUM(ri.quantity) FROM refund_items ri
			JOIN refunds r ON r.gym_id=ri.gym_id AND r.id=ri.refund_id
			WHERE ri.gym_id=si.gym_id AND ri.sale_item_id=si.id AND ri.deleted_at IS NULL
			  AND r.sale_id=? AND r.kind='revenue_refund' AND r.deleted_at IS NULL),0)::integer AS refunded_qty,
		COALESCE((SELECT SUM(ROUND(ri.amount*100)::bigint) FROM refund_items ri
			JOIN refunds r ON r.gym_id=ri.gym_id AND r.id=ri.refund_id
			WHERE ri.gym_id=si.gym_id AND ri.sale_item_id=si.id AND ri.deleted_at IS NULL
			  AND r.sale_id=? AND r.kind='revenue_refund' AND r.deleted_at IS NULL),0)::bigint AS refunded_cents
		FROM sale_items si WHERE si.gym_id=? AND si.sale_id=? AND si.deleted_at IS NULL
		ORDER BY si.id FOR UPDATE OF si`, saleID, saleID, gymID, saleID).Scan(&lines).Error; err != nil {
		return fmt.Errorf("validar asignación de productos devueltos: %w", err)
	}
	if len(lines) == 0 {
		return newFinancialConflict("la venta devuelta no conserva productos")
	}
	var runningSubtotal, previousAllocation int64
	for index, line := range lines {
		if line.Quantity <= 0 || line.LineTotalCents <= 0 || line.RefundedQty < 0 || line.RefundedQty > line.Quantity {
			return newFinancialConflict("la cantidad devuelta excede lo vendido para uno de los productos")
		}
		runningSubtotal += line.LineTotalCents
		cumulativeAllocation := sale.TotalCents
		if index < len(lines)-1 {
			cumulativeAllocation = roundedRatioExact(runningSubtotal, sale.TotalCents, sale.SubtotalCents)
		}
		lineAllocation := cumulativeAllocation - previousAllocation
		previousAllocation = cumulativeAllocation
		expectedRefunded := roundedRatioExact(int64(line.RefundedQty), lineAllocation, int64(line.Quantity))
		if line.RefundedCents != expectedRefunded {
			return newFinancialConflict(
				"la asignación devuelta por producto no coincide: esperaba %s y recibió %s",
				formatProjectorCents(expectedRefunded), formatProjectorCents(line.RefundedCents),
			)
		}
	}
	return nil
}

// roundedRatioExact computes round(value*multiplier/divisor) with halves away
// from zero for non-negative money, without overflowing int64 intermediates.
func roundedRatioExact(value, multiplier, divisor int64) int64 {
	if value <= 0 || multiplier <= 0 || divisor <= 0 {
		return 0
	}
	numerator := new(big.Int).Mul(big.NewInt(value), big.NewInt(multiplier))
	numerator.Mul(numerator, big.NewInt(2))
	numerator.Add(numerator, big.NewInt(divisor))
	denominator := new(big.Int).Mul(big.NewInt(divisor), big.NewInt(2))
	return new(big.Int).Quo(numerator, denominator).Int64()
}
