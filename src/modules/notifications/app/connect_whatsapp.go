package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ConnectWhatsAppInput backs UC-037. Phone is the gym's WhatsApp Business
// number in E.164 — owner enters it after the verification ceremony with
// Twilio finishes. The provider returns a SenderID we keep for downstream
// API calls.
type ConnectWhatsAppInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Phone       string
}

type ConnectWhatsAppOutput struct {
	GymID       uuid.UUID
	Phone       string
	SenderID    string
	ConnectedAt time.Time
}

// ConnectWhatsApp reaches into the gyms BC to record the connection state.
// Cross-BC by design: the Gym aggregate owns the (Phone, ConnectedAt) pair
// and notifications BC owns the provider integration. Audit row is recorded
// against `gyms` since that's the entity that mutated.
type ConnectWhatsApp struct {
	Gyms     gymRepo.GymRepository
	Provider notiDomain.WhatsAppProvider
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewConnectWhatsApp(gyms gymRepo.GymRepository, provider notiDomain.WhatsAppProvider,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *ConnectWhatsApp {
	return &ConnectWhatsApp{Gyms: gyms, Provider: provider, UoW: uow, Audit: recorder}
}

func (uc *ConnectWhatsApp) Execute(ctx context.Context, in ConnectWhatsAppInput) (*ConnectWhatsAppOutput, error) {
	phone := strings.TrimSpace(in.Phone)
	if phone == "" {
		return nil, sharedDomain.NewValidationError(notiErrors.ErrInvalidPhone)
	}
	now := time.Now().UTC()

	// Provider call happens BEFORE the transaction so we don't hold a DB
	// connection during a network round-trip. Failures here are surfaced as
	// "provider unavailable" — the controller can show a retry hint.
	senderID, err := uc.Provider.RegisterSender(ctx, phone)
	if err != nil {
		return nil, sharedDomain.NewBusinessError(notiErrors.ErrProviderUnavailable, err.Error())
	}

	var out ConnectWhatsAppOutput
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		g, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		// Token bytes (Twilio sub-account creds) are out of scope for MVP —
		// we keep the column null and store senderID alongside in audit.
		if err := g.ConnectWhatsApp(phone, nil, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Gyms.Update(tx, g)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       g.ID,
			EntityType:  "gyms",
			EntityID:    g.ID,
			Action:      "whatsapp_connect",
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"whatsapp_phone": phone,
				"sender_id":      senderID,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = ConnectWhatsAppOutput{
			GymID:       updated.ID,
			Phone:       *updated.WhatsAppBusinessPhone,
			SenderID:    senderID,
			ConnectedAt: *updated.WhatsAppConnectedAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWhatsAppStatusInput is a read-only fetch — no use case object needed,
// but kept symmetric for the controller wiring.
type GetWhatsAppStatusInput struct {
	GymID uuid.UUID
}

type GetWhatsAppStatusOutput struct {
	Connected   bool
	Phone       *string
	ConnectedAt *time.Time
}

type GetWhatsAppStatus struct {
	Gyms gymRepo.GymRepository
	UoW  sharedDomain.UnitOfWork
}

func NewGetWhatsAppStatus(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork) *GetWhatsAppStatus {
	return &GetWhatsAppStatus{Gyms: gyms, UoW: uow}
}

func (uc *GetWhatsAppStatus) Execute(ctx context.Context, in GetWhatsAppStatusInput) (*GetWhatsAppStatusOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	g, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return nil, err
	}
	return &GetWhatsAppStatusOutput{
		Connected:   g.IsWhatsAppConnected(),
		Phone:       g.WhatsAppBusinessPhone,
		ConnectedAt: g.WhatsAppConnectedAt,
	}, nil
}
