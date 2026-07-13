//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// Round-trip completo del incidente ×100 de la migración HDLEON (jul-2026):
// journal (PESOS, como lo emite import-gym post-#15) → ApplyPullChange sobre
// las migraciones sqlite reales → columna INTEGER en centavos → reader de
// reports (SumPaymentsBetween, el "ingresos del mes" del desktop) → PESOS
// otra vez. Si cualquier salto de la cadena vuelve a multiplicar o dividir
// de más, este test truena con el factor exacto.
//
// El segundo test fija la SANACIÓN: una fila envenenada (el journal en
// centavos que emitió el binario stale pre-#15 aterriza como centavos² en
// SQLite) NO se corrige re-emitiendo con version=1 (el LWW descarta
// versiones no-mayores — por eso el primer intento de recuperación falló
// aunque el journal ya estuviera en pesos), y SÍ se corrige con la versión
// bumpeada que nextImportVersion (cmd/import-gym) emite ahora.

const pesosPagoPiloto = 550.0

// paymentWirePayload arma el payload de un payment con la misma forma que
// import-gym emite al journal. amountWire va TAL CUAL al wire: pesos en el
// flujo sano, centavos si se simula el journal envenenado.
func paymentWirePayload(t *testing.T, paymentID string, gymID, operatorID uuid.UUID, version int, amountWire float64) []byte {
	t.Helper()
	now := time.Now().UTC()
	payload, err := json.Marshal(map[string]any{
		"id":              paymentID,
		"gym_id":          gymID.String(),
		"version":         version,
		"created_at":      now.UnixMilli(),
		"updated_at":      now.UnixMilli(),
		"folio":           "P-550",
		"member_id":       nil,
		"amount":          amountWire,
		"payment_method":  "cash",
		"concept":         "membership",
		"discount_amount": 0,
		"balance_pending": 0,
		"payment_date":    now.Format("2006-01-02"),
		"operator_id":     operatorID.String(),
	})
	if err != nil {
		t.Fatalf("marshal payment payload: %v", err)
	}
	return payload
}

func applyPaymentChange(t *testing.T, uow sharedDomain.UnitOfWork, paymentID string, version int, payload []byte) {
	t.Helper()
	change := syncpkg.PullChange{
		EntityType: "payments", EntityID: paymentID, Version: version,
		Payload: payload, ServerUpdatedAt: time.Now().UTC(),
	}
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, change)
	}); err != nil {
		t.Fatalf("apply payment v%d: %v", version, err)
	}
}

func monthIncome(t *testing.T, uow sharedDomain.UnitOfWork, gymID uuid.UUID) float64 {
	t.Helper()
	tx, err := uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	income, err := reportsInfra.NewSQLiteReader().SumPaymentsBetween(tx, gymID,
		time.Now().UTC().Add(-48*time.Hour), time.Now().UTC().Add(48*time.Hour))
	if err != nil {
		t.Fatalf("SumPaymentsBetween: %v", err)
	}
	return income
}

func sqliteAmountCents(t *testing.T, uow sharedDomain.UnitOfWork, paymentID string) int64 {
	t.Helper()
	tx, err := uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	var cents int64
	if err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &cents,
		`SELECT amount FROM payments WHERE id = ?`, paymentID); err != nil {
		t.Fatalf("leer amount: %v", err)
	}
	return cents
}

func TestReimportMoneyRoundTrip_JournalPesosLlegaAlReaderComoPesos(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	operatorID := uuid.New()
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, ?, ?, 'op@t.mx', 'x', 'Operador', 'operator')`,
		operatorID, gymID, now, now); err != nil {
		t.Fatalf("seed operator: %v", err)
	}

	paymentID := uuid.New().String()
	applyPaymentChange(t, uow, paymentID, 1,
		paymentWirePayload(t, paymentID, gymID, operatorID, 1, pesosPagoPiloto))

	if cents := sqliteAmountCents(t, uow, paymentID); cents != 55000 {
		t.Errorf("SQLite amount = %d centavos, want 55000 ($550 × 100)", cents)
	}
	if income := monthIncome(t, uow, gymID); income != pesosPagoPiloto {
		t.Errorf("ingresos del reader = %v, want %v — factor %.0f (el incidente era ×100)",
			income, pesosPagoPiloto, income/pesosPagoPiloto)
	}
}

func TestReimportMoneyRoundTrip_FilaEnvenenadaSanaSoloConVersionBump(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	operatorID := uuid.New()
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, ?, ?, 'op@t.mx', 'x', 'Operador', 'operator')`,
		operatorID, gymID, now, now); err != nil {
		t.Fatalf("seed operator: %v", err)
	}

	paymentID := uuid.New().String()

	// 1. El journal envenenado del binario stale: CENTAVOS en el wire.
	//    El apply (correctamente) multiplica ×100 → centavos² en SQLite.
	applyPaymentChange(t, uow, paymentID, 1,
		paymentWirePayload(t, paymentID, gymID, operatorID, 1, pesosPagoPiloto*100))
	if income := monthIncome(t, uow, gymID); income != pesosPagoPiloto*100 {
		t.Fatalf("precondición: el reader debía ver ×100 (%v), got %v", pesosPagoPiloto*100, income)
	}

	// 2. Recuperación VIEJA: re-emisión corregida (pesos) pero con version=1.
	//    El LWW local (local.version >= change.version → skip) la descarta:
	//    la fila envenenada sobrevive. Exactamente por esto el gym piloto
	//    seguía ×100 tras re-correr el import sin bump de versión.
	applyPaymentChange(t, uow, paymentID, 1,
		paymentWirePayload(t, paymentID, gymID, operatorID, 1, pesosPagoPiloto))
	if income := monthIncome(t, uow, gymID); income != pesosPagoPiloto*100 {
		t.Fatalf("la re-emisión v1 no debía aplicar (LWW), got %v", income)
	}

	// 3. Recuperación NUEVA: nextImportVersion emite por encima de todo lo
	//    visto (aquí 1+5=6). El pull incremental la acepta y pisa la fila
	//    envenenada — sin wipe manual de AppData.
	applyPaymentChange(t, uow, paymentID, 6,
		paymentWirePayload(t, paymentID, gymID, operatorID, 6, pesosPagoPiloto))
	if cents := sqliteAmountCents(t, uow, paymentID); cents != 55000 {
		t.Errorf("tras el re-import bumpeado: amount = %d centavos, want 55000", cents)
	}
	if income := monthIncome(t, uow, gymID); income != pesosPagoPiloto {
		t.Errorf("tras el re-import bumpeado: reader = %v, want %v", income, pesosPagoPiloto)
	}
}
