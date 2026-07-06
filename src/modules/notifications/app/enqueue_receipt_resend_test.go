package app

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Estos tests pinean la semántica del reenvío manual de comprobante
// (UC-020) que el dogfood destapó: el botón "Enviar por WhatsApp" era un
// no-op silencioso para cualquier pago cuyo recibo automático ya se
// hubiera encolado — el dedup por receipt:<paymentID> devolvía la fila
// existente y el FE cantaba éxito. Semántica pineada:
//   - camino automático (Resend=false): un cobro = un recibo, para siempre.
//   - resend con fila pending: NO duplica (dos WhatsApps + doble costo del
//     número maestro); reporta AlreadyPending.
//   - resend con fila sent/held/failed: re-arma la MISMA fila (una fila
//     por pago — los reenvíos offline no se apilan) con teléfono y
//     CreatedAt de HOY (el guard anti-stale ancla en CreatedAt).
//   - socio sin teléfono: skip explícito, no silencioso.

// fakes por embedding: sólo se overridean los métodos que EnqueueReceipt
// usa; cualquier otro panicearía (interface embebida nil) y delataría un
// cambio de dependencias del use case.

type fakeNotiRepo struct {
	notiRepo.NotificationRepository
	byKey   map[string]*notiDomain.Notification
	created []*notiDomain.Notification
	updated []*notiDomain.Notification
}

func (f *fakeNotiRepo) GetByIdempotencyKey(_ sharedDomain.Transaction, _ uuid.UUID, key string) (*notiDomain.Notification, error) {
	return f.byKey[key], nil
}

func (f *fakeNotiRepo) Create(_ sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error) {
	f.created = append(f.created, n)
	return n, nil
}

func (f *fakeNotiRepo) Update(_ sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error) {
	f.updated = append(f.updated, n)
	return n, nil
}

type fakeTemplateRepo struct {
	notiRepo.TemplateOverrideRepository
	byKey map[string]*tplDomain.Override
}

func (f *fakeTemplateRepo) GetByGymAndKey(_ sharedDomain.Transaction, _ uuid.UUID, key string) (*tplDomain.Override, error) {
	return f.byKey[key], nil
}

type fakeGymRepo struct {
	gymRepo.GymRepository
	gym *gymDomain.Gym
}

func (f *fakeGymRepo) GetByID(_ sharedDomain.Transaction, _ uuid.UUID) (*gymDomain.Gym, error) {
	return f.gym, nil
}

type fakeMemberRepo struct {
	memRepo.MemberRepository
	member *memDomain.Member
}

func (f *fakeMemberRepo) GetByID(_ sharedDomain.Transaction, _ uuid.UUID) (*memDomain.Member, error) {
	return f.member, nil
}

type fakeTx struct{}

func (fakeTx) Execute(fn func(tx sharedDomain.Transaction) error) error { return fn(fakeTx{}) }

type fakeUoW struct{}

func (fakeUoW) Begin(context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Commit(sharedDomain.Transaction) error                   { return nil }
func (fakeUoW) Rollback(sharedDomain.Transaction) error                 { return nil }
func (fakeUoW) Query(context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Command(_ context.Context, fn func(tx sharedDomain.Transaction) error) error {
	return fn(fakeTx{})
}

func receiptFixture(t *testing.T) (uc *EnqueueReceipt, notis *fakeNotiRepo, in EnqueueReceiptInput) {
	t.Helper()
	gymID, memberID, paymentID := uuid.New(), uuid.New(), uuid.New()
	name := "Gym Pilot"
	notis = &fakeNotiRepo{byKey: map[string]*notiDomain.Notification{}}
	uc = NewEnqueueReceipt(
		notis,
		&fakeGymRepo{gym: &gymDomain.Gym{ID: gymID, Name: &name}},
		&fakeMemberRepo{member: &memDomain.Member{
			ID: memberID, GymID: gymID, FullName: "Rosa Robles", Phone: "5522334455",
		}},
		// Sin overrides — el default de todo template es enabled.
		&fakeTemplateRepo{byKey: map[string]*tplDomain.Override{}},
		fakeUoW{},
	)
	in = EnqueueReceiptInput{
		GymID: gymID, PaymentID: paymentID, MemberID: memberID,
		Concept: "membership", Amount: 500, Folio: "F-001",
	}
	return uc, notis, in
}

// sentRow arma la fila base ya despachada (status sent) con datos VIEJOS,
// para verificar que el re-arm los refresca.
func sentRow(in EnqueueReceiptInput, old time.Time) *notiDomain.Notification {
	key := "receipt:" + in.PaymentID.String()
	sentAt := old.Add(time.Minute)
	return &notiDomain.Notification{
		ID: uuid.New(), GymID: in.GymID, Version: 3,
		Channel: notiDomain.ChannelWhatsApp, TemplateKey: "receipt_membership",
		RecipientType: notiDomain.RecipientMember, RecipientID: in.MemberID,
		RecipientAddress: "5500000000", // teléfono viejo — el socio lo corrigió después
		Payload:          map[string]string{"amount": "500.00"},
		Status:           notiDomain.StatusSent, SentAt: &sentAt,
		RetryCount: 2, ScheduledFor: old, IdempotencyKey: &key,
		CreatedAt: old, UpdatedAt: old,
	}
}

func TestEnqueueReceipt_AutoDedupePorPago(t *testing.T) {
	uc, notis, in := receiptFixture(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	existing := sentRow(in, old)
	notis.byKey["receipt:"+in.PaymentID.String()] = existing

	out, err := uc.Execute(context.Background(), in) // Resend=false
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(notis.created) != 0 || len(notis.updated) != 0 {
		t.Errorf("camino automático NO debe crear (%d) ni tocar (%d) filas", len(notis.created), len(notis.updated))
	}
	if out.NotificationID == nil || *out.NotificationID != existing.ID {
		t.Errorf("debe devolver la fila existente, got %v", out.NotificationID)
	}
}

func TestEnqueueReceipt_ResendConPendingNoDuplica(t *testing.T) {
	uc, notis, in := receiptFixture(t)
	in.Resend = true
	existing := sentRow(in, time.Now().UTC().Add(-time.Minute))
	existing.Status = notiDomain.StatusPending
	existing.SentAt = nil
	notis.byKey["receipt:"+in.PaymentID.String()] = existing

	out, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !out.AlreadyPending {
		t.Error("fila pending → AlreadyPending=true (ya va en camino, no se duplica)")
	}
	if len(notis.created) != 0 || len(notis.updated) != 0 {
		t.Errorf("no debe crear (%d) ni re-armar (%d) sobre una fila en vuelo", len(notis.created), len(notis.updated))
	}
}

func TestEnqueueReceipt_ResendConSentRearmaLaMismaFila(t *testing.T) {
	uc, notis, in := receiptFixture(t)
	in.Resend = true
	testStart := time.Now().UTC().Add(-time.Second)
	old := time.Now().UTC().Add(-72 * time.Hour) // recibo de hace 3 días
	existing := sentRow(in, old)
	notis.byKey["receipt:"+in.PaymentID.String()] = existing

	out, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(notis.created) != 0 {
		t.Fatalf("re-arm no debe crear filas nuevas, got %d", len(notis.created))
	}
	if len(notis.updated) != 1 || notis.updated[0].ID != existing.ID {
		t.Fatalf("debe re-armar exactamente la fila existente, got %+v", notis.updated)
	}
	if out.AlreadyPending {
		t.Error("re-arm real no es AlreadyPending")
	}
	if existing.Status != notiDomain.StatusPending || existing.SentAt != nil {
		t.Errorf("re-arm: status=%q sent_at=%v, want pending/nil", existing.Status, existing.SentAt)
	}
	// CreatedAt refrescado — sin esto el guard anti-stale (TTL 1 día para
	// recibos, anclado en CreatedAt) mandaría este reenvío a `held` al
	// instante por ser de hace 3 días.
	if existing.CreatedAt.Before(testStart) {
		t.Errorf("CreatedAt sin refrescar (%v) — el reenvío caería held por stale", existing.CreatedAt)
	}
	// Teléfono de HOY, no el del cobro (caso típico: se lo corrigieron).
	if existing.RecipientAddress != "5522334455" {
		t.Errorf("RecipientAddress = %q, want el teléfono actual del socio", existing.RecipientAddress)
	}
	if existing.RetryCount != 2 {
		t.Errorf("RetryCount debe conservarse como señal histórica, got %d", existing.RetryCount)
	}
}

func TestEnqueueReceipt_ResendSinFilaCreaConLlaveBase(t *testing.T) {
	uc, notis, in := receiptFixture(t)
	in.Resend = true // el recibo nunca se encoló (p.ej. no tenía teléfono al cobrar)

	out, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(notis.created) != 1 {
		t.Fatalf("debe crear la fila base, got %d", len(notis.created))
	}
	wantKey := "receipt:" + in.PaymentID.String()
	if k := notis.created[0].IdempotencyKey; k == nil || *k != wantKey {
		t.Errorf("idempotency_key = %v, want %q", k, wantKey)
	}
	if out.Skipped || out.AlreadyPending {
		t.Errorf("primer envío limpio: skipped=%v already_pending=%v", out.Skipped, out.AlreadyPending)
	}
}

func TestEnqueueReceipt_ResendSinTelefonoReportaSkip(t *testing.T) {
	uc, notis, in := receiptFixture(t)
	in.Resend = true
	// receiptFixture comparte el puntero del member — vaciamos el teléfono.
	uc.Members.(*fakeMemberRepo).member.Phone = "  "

	out, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !out.Skipped || out.SkippedReason != "no_member_phone" {
		t.Errorf("skip = %v/%q, want true/no_member_phone", out.Skipped, out.SkippedReason)
	}
	if len(notis.created) != 0 && len(notis.updated) != 0 {
		t.Error("sin teléfono no debe encolar nada")
	}
}

// disableTemplate apaga el template en el fake — equivale al toggle de
// Ajustes → Mensajes en off (la copia local del sidecar, donde el dueño
// lo edita).
func disableTemplate(uc *EnqueueReceipt, key string) {
	tpls := uc.Templates.(*fakeTemplateRepo)
	tpls.byKey[key] = &tplDomain.Override{
		ID: uuid.New(), TemplateKey: key, Enabled: false,
	}
}

func TestEnqueueReceipt_ResendConTemplateApagadoReportaSkip(t *testing.T) {
	uc, notis, in := receiptFixture(t)
	in.Resend = true
	disableTemplate(uc, "receipt_membership")

	out, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !out.Skipped || out.SkippedReason != "template_disabled" {
		t.Errorf("toggle apagado → skip explícito template_disabled, got %+v", out)
	}
	if len(notis.created) != 0 || len(notis.updated) != 0 {
		t.Error("con el template apagado no debe encolar ni re-armar nada")
	}
}

func TestEnqueueReceipt_AutoConTemplateApagadoNoEncola(t *testing.T) {
	uc, notis, in := receiptFixture(t)
	disableTemplate(uc, "receipt_membership")

	out, err := uc.Execute(context.Background(), in) // Resend=false
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !out.Skipped || out.SkippedReason != "template_disabled" {
		t.Errorf("camino automático también respeta el toggle, got %+v", out)
	}
	if len(notis.created) != 0 {
		t.Error("no debe crear filas — el dispatcher las marcaría failed y ensucian la bitácora")
	}
}

func TestEnqueueReceipt_TemplateDeOtroConceptoApagadoNoAfecta(t *testing.T) {
	uc, notis, in := receiptFixture(t)
	// El toggle es por template: apagar el de PRODUCTO no bloquea el
	// comprobante de MEMBRESÍA.
	disableTemplate(uc, "receipt_product")

	out, err := uc.Execute(context.Background(), in) // Concept: membership
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.Skipped {
		t.Errorf("template de membresía sigue encendido, got skip %q", out.SkippedReason)
	}
	if len(notis.created) != 1 {
		t.Errorf("debe encolar el recibo de membresía, created=%d", len(notis.created))
	}
}
