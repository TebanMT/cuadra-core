package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// GenerateReceiptInput backs UC-020 (PDF download). The receipt is rendered
// in-memory; we never store it (DA-20.1).
type GenerateReceiptInput struct {
	GymID     uuid.UUID
	PaymentID uuid.UUID
}

// GenerateReceiptOutput is what the controller streams back. ContentType is
// always "application/pdf"; SuggestedFilename hints to Content-Disposition.
type GenerateReceiptOutput struct {
	PDF               []byte
	ContentType       string
	SuggestedFilename string
}

type GenerateReceipt struct {
	Payments billingRepo.PaymentRepository
	Gyms     gymRepo.GymRepository
	Members  memRepo.MemberRepository
	UoW      sharedDomain.UnitOfWork
	// Opcionales (With*): sin ellos el comprobante sale sin vigencia, sin
	// "Atendió" o sin logo, pero sale.
	Memberships memRepo.MembershipRepository
	Users       userRepo.UserRepository
	Logos       LogoSource
}

// LogoSource entrega el logo del gym normalizado a PNG (ver
// shared/logocache). Cualquier error = comprobante sin logo.
type LogoSource interface {
	PNG(ctx context.Context, gymID, rawURL string) ([]byte, error)
}

func NewGenerateReceipt(payments billingRepo.PaymentRepository, gyms gymRepo.GymRepository,
	members memRepo.MemberRepository, uow sharedDomain.UnitOfWork) *GenerateReceipt {
	return &GenerateReceipt{Payments: payments, Gyms: gyms, Members: members, UoW: uow}
}

// WithMemberships habilita plan + vigencia (inicio / vence) en el
// comprobante de inscripción o renovación, y distingue inscripción de
// renovación cuando el gym no cobra inscripción.
func (uc *GenerateReceipt) WithMemberships(r memRepo.MembershipRepository) *GenerateReceipt {
	uc.Memberships = r
	return uc
}

// WithUsers habilita la línea "Atendió: <operador>".
func (uc *GenerateReceipt) WithUsers(r userRepo.UserRepository) *GenerateReceipt {
	uc.Users = r
	return uc
}

// WithLogos habilita el logo del gym en el PDF y en /receipts/logo.
func (uc *GenerateReceipt) WithLogos(l LogoSource) *GenerateReceipt {
	uc.Logos = l
	return uc
}

// View arma el contenido del comprobante (GET /payments/:id/receipt). Es la
// misma vista que renderiza el PDF: ticket impreso, dashboard y WhatsApp
// siempre coinciden.
func (uc *GenerateReceipt) View(ctx context.Context, in GenerateReceiptInput) (*ReceiptView, error) {
	v, _, err := uc.view(ctx, in)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (uc *GenerateReceipt) view(ctx context.Context, in GenerateReceiptInput) (*ReceiptView, *gymDomain.Gym, error) {
	tx, err := uc.UoW.Query(ctx)
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		return nil, nil, sharedDomain.NewUnexpectedError(err)
	}
	p, err := uc.Payments.GetByID(tx, in.PaymentID)
	if err != nil {
		return nil, nil, err
	}
	if p.GymID != in.GymID {
		return nil, nil, sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
	}
	g, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return nil, nil, err
	}
	ri := receiptInputs{gym: g, payment: p, hasLogo: g.LogoURL != nil && strings.TrimSpace(*g.LogoURL) != ""}
	if p.MemberID != nil {
		if m, err := uc.Members.GetByID(tx, *p.MemberID); err == nil {
			ri.member = m
		}
	}
	if uc.Memberships != nil && p.MembershipID != nil {
		if ms, err := uc.Memberships.GetByID(tx, *p.MembershipID); err == nil && ms != nil && ms.GymID == p.GymID {
			ri.membership = ms
			if prev, err := uc.Memberships.GetReplacedBy(tx, ms.ID); err == nil && prev == nil {
				ri.firstMembership = true
			}
		}
	}
	if uc.Users != nil && p.OperatorID != uuid.Nil {
		if u, err := uc.Users.GetByID(tx, p.OperatorID); err == nil && u != nil && u.GymID == p.GymID {
			ri.operatorName = u.FullName
		}
	}
	v := buildReceiptView(ri)
	return &v, g, nil
}

func (uc *GenerateReceipt) Execute(ctx context.Context, in GenerateReceiptInput) (*GenerateReceiptOutput, error) {
	v, g, err := uc.view(ctx, in)
	if err != nil {
		return nil, err
	}
	var logo []byte
	if v.Gym.HasLogo {
		logo, _ = uc.LogoPNG(ctx, g)
	}
	pdf, err := renderReceiptPDF(*v, logo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &GenerateReceiptOutput{
		PDF:               pdf,
		ContentType:       "application/pdf",
		SuggestedFilename: fmt.Sprintf("recibo-%s.pdf", strings.ReplaceAll(v.Folio, "/", "-")),
	}, nil
}

// LogoPNG devuelve el logo normalizado del gym (para el ticket impreso, que
// lo pide por GET /receipts/logo). nil, nil = el gym no tiene logo.
func (uc *GenerateReceipt) LogoPNG(ctx context.Context, g *gymDomain.Gym) ([]byte, error) {
	if uc.Logos == nil || g == nil || g.LogoURL == nil || strings.TrimSpace(*g.LogoURL) == "" {
		return nil, nil
	}
	return uc.Logos.PNG(ctx, g.ID.String(), *g.LogoURL)
}

// GymLogoPNG resuelve el gym y su logo en un paso (handler de /receipts/logo).
func (uc *GenerateReceipt) GymLogoPNG(ctx context.Context, gymID uuid.UUID) ([]byte, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		sharedDomain.CloseQuery(tx)
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	g, err := uc.Gyms.GetByID(tx, gymID)
	sharedDomain.CloseQuery(tx)
	if err != nil {
		return nil, err
	}
	return uc.LogoPNG(ctx, g)
}

// SendReceiptInput backs the manual UC-020 "Reenviar comprobante" call. El
// envío se delega al seam de notificaciones (ReceiptResender →
// BillingEventSubscriber → EnqueueReceipt en modo resend), el MISMO mapeo
// que dispara el cobro automático pero con veredicto de vuelta.
// Channel/Recipient quedan para overrides futuros (otro número / email);
// hoy el envío default es WhatsApp al socio del pago vía master sender.
type SendReceiptInput struct {
	GymID     uuid.UUID
	PaymentID uuid.UUID
	Channel   string // "whatsapp_member" | "whatsapp_other" | "email_member" | "email_other"
	Recipient string // optional override (phone or email)
}

type SendReceiptOutput struct {
	Status string
	Note   string
}

type SendReceipt struct {
	Payments  billingRepo.PaymentRepository
	Publisher EventPublisher
	Resender  ReceiptResender
	UoW       sharedDomain.UnitOfWork
}

func NewSendReceipt(payments billingRepo.PaymentRepository, uow sharedDomain.UnitOfWork) *SendReceipt {
	return &SendReceipt{Payments: payments, Publisher: NoopPublisher{}, UoW: uow}
}

// WithPublisher inyecta el seam de notificaciones (el MISMO
// BillingEventSubscriber que usa el cobro automático para el comprobante).
// Sin él, SendReceipt degrada a NoopPublisher (no-op silencioso) — útil en
// tests que no montan la BC de notificaciones.
func (uc *SendReceipt) WithPublisher(p EventPublisher) *SendReceipt {
	if p != nil {
		uc.Publisher = p
	}
	return uc
}

// WithResender inyecta el camino con retorno del reenvío manual. Cuando
// está cableado, Execute reporta el veredicto real (encolado / ya en
// camino / sin teléfono) en lugar del "queued" ciego del publisher.
func (uc *SendReceipt) WithResender(r ReceiptResender) *SendReceipt {
	if r != nil {
		uc.Resender = r
	}
	return uc
}

func (uc *SendReceipt) Execute(ctx context.Context, in SendReceiptInput) (*SendReceiptOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	p, err := uc.Payments.GetByID(tx, in.PaymentID)
	if err != nil {
		return nil, err
	}
	if p.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
	}
	if p.MemberID == nil {
		// Venta anónima sin socio: no hay a quién mandarle el comprobante.
		return &SendReceiptOutput{
			Status: "skipped",
			Note:   "El pago no tiene socio asociado.",
		}, nil
	}
	// Reusa el MISMO mapeo de evento que el cobro automático, pero por el
	// camino CON retorno (ReceiptResender → EnqueueReceipt en modo resend):
	// si el comprobante del pago sigue pendiente en la cola no duplica ("ya
	// en camino"); si ya salió (o quedó held/failed) encola un envío NUEVO;
	// si el socio no tiene teléfono lo dice en vez de fingir éxito. El
	// dedup ciego de antes (idemp key receipt:<paymentID> a secas) hacía
	// que el botón "Enviar por WhatsApp" fuera un no-op silencioso para
	// cualquier pago cuyo recibo automático ya hubiera salido. El
	// dispatcher resuelve el `from` por tier (master sender en Standard).
	// Query() arriba devuelve un tx read-only sin lock (Tx=nil), así que el
	// Command de EnqueueReceipt no choca.
	// The product receipt message describes the purchase. A fully financed
	// purchase has a positive sale total even though its initial collection is zero.
	receiptAmount := p.Amount
	if p.Concept == paymentDomain.ConceptProduct && p.Amount == 0 {
		for _, line := range p.Breakdown {
			receiptAmount += line.Amount
		}
		receiptAmount -= p.DiscountAmount
	}
	evt := PaymentCompletedEvent{
		GymID:      p.GymID,
		PaymentID:  in.PaymentID,
		MemberID:   p.MemberID,
		Concept:    p.Concept,
		Amount:     receiptAmount,
		Folio:      p.Folio,
		OperatorID: p.OperatorID,
	}
	if uc.Resender == nil {
		// Fallback sin resender cableado (tests viejos): fire-and-forget.
		uc.Publisher.PublishPaymentCompleted(ctx, evt)
		return &SendReceiptOutput{
			Status: "queued",
			Note:   "Comprobante encolado para envío por WhatsApp.",
		}, nil
	}
	outcome, err := uc.Resender.ResendReceipt(ctx, evt)
	if err != nil {
		return nil, err
	}
	switch outcome.Status {
	case "already_pending":
		return &SendReceiptOutput{
			Status: "already_pending",
			Note:   "El comprobante ya está en camino — le llega por WhatsApp en unos minutos.",
		}, nil
	case "skipped":
		note := "No se pudo enviar el comprobante."
		switch outcome.Reason {
		case "no_member_phone":
			note = "El socio no tiene teléfono registrado. Agrégalo en su perfil y vuelve a intentar."
		case "template_disabled":
			// El dueño apagó el template del comprobante en Ajustes →
			// Mensajes. Antes esto se descubría hasta el dispatcher del
			// cloud y el operador veía un "en camino" mentiroso.
			note = "Los comprobantes por WhatsApp están desactivados. Actívalos en Ajustes → Mensajes."
		}
		return &SendReceiptOutput{Status: "skipped", Note: note}, nil
	default:
		return &SendReceiptOutput{
			Status: "queued",
			Note:   "Comprobante enviado a la cola — le llega por WhatsApp en unos minutos.",
		}, nil
	}
}

// buildReceiptLines aplana el comprobante a texto (pruebas de la semántica
// de montos). Firma histórica: datos del gym sueltos + pago + socio.
func buildReceiptLines(gymName, city, whatsapp, rfc, razon *string,
	p *paymentDomain.Payment, memberName, memberFolio string) []string {
	g := &gymDomain.Gym{Name: gymName, City: city, WhatsApp: whatsapp, RFC: rfc, RazonSocial: razon}
	ri := receiptInputs{gym: g, payment: p}
	if memberName != "" {
		ri.member = &memberDomain.Member{FullName: memberName, Folio: memberFolio}
	}
	return receiptTextLines(buildReceiptView(ri))
}

func conceptLabel(c string) string {
	switch c {
	case paymentDomain.ConceptMembership:
		return "Mensualidad / membresía"
	case paymentDomain.ConceptProduct:
		return "Venta de producto"
	case paymentDomain.ConceptBalanceSettlement:
		return "Liquidación de saldo"
	case paymentDomain.ConceptRefund:
		return "Cancelación / devolución"
	case paymentDomain.ConceptOther:
		return "Ingreso extraordinario"
	}
	return c
}

func methodLabel(m string) string {
	switch m {
	case paymentDomain.MethodCash:
		return "Efectivo"
	case paymentDomain.MethodTransfer:
		return "Transferencia"
	case paymentDomain.MethodCard:
		return "Tarjeta"
	}
	return m
}
