package app

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/accessdevice"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// TestAccessActuatorInput backs the owner-only hardware smoke test. It is not
// a check-in and therefore never creates attendance for a member.
type TestAccessActuatorInput struct {
	GymID    uuid.UUID
	OwnerID  uuid.UUID
	Duration time.Duration
}

type TestAccessActuatorOutput struct {
	Receipt       accessdevice.Receipt
	AuditRecorded bool
}

// TestAccessActuator proves the local execution path before it is connected
// to real access decisions: API -> use case -> device -> ACK -> audit.
type TestAccessActuator struct {
	Actuator accessdevice.Actuator
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewTestAccessActuator(
	actuator accessdevice.Actuator,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *TestAccessActuator {
	return &TestAccessActuator{Actuator: actuator, UoW: uow, Audit: recorder}
}

func (uc *TestAccessActuator) Execute(ctx context.Context, in TestAccessActuatorInput) (TestAccessActuatorOutput, error) {
	if in.GymID == uuid.Nil || in.OwnerID == uuid.Nil {
		return TestAccessActuatorOutput{}, sharedDomain.NewValidationError(accessdevice.ErrUnavailable)
	}
	if in.Duration < accessdevice.MinPulseDuration || in.Duration > accessdevice.MaxPulseDuration {
		return TestAccessActuatorOutput{}, sharedDomain.NewValidationError(accessdevice.ErrInvalidDuration)
	}
	if uc.Actuator == nil {
		return TestAccessActuatorOutput{}, sharedDomain.NewBusinessError(accessdevice.ErrUnavailable, "no hay un adaptador configurado")
	}

	receipt, err := uc.Actuator.Pulse(ctx, accessdevice.PulseCommand{
		ID:       uuid.New(),
		Duration: in.Duration,
	})
	if err != nil {
		return TestAccessActuatorOutput{}, sharedDomain.NewBusinessError(accessdevice.ErrUnavailable, err.Error())
	}

	// The physical action already happened. If the audit write fails, returning
	// an HTTP error would invite the caller to retry and pulse twice. Preserve
	// the successful receipt, expose audit_recorded=false and log loudly.
	audited := uc.recordAudit(ctx, in, receipt)
	return TestAccessActuatorOutput{Receipt: receipt, AuditRecorded: audited}, nil
}

func (uc *TestAccessActuator) recordAudit(
	ctx context.Context,
	in TestAccessActuatorInput,
	receipt accessdevice.Receipt,
) bool {
	if uc.UoW == nil || uc.Audit == nil {
		return false
	}
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		ownerID := in.OwnerID
		return uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "access_device_commands",
			EntityID:    receipt.CommandID,
			Action:      "test_pulse",
			ActorUserID: &ownerID,
			Changes: map[string]any{
				"device_id":    receipt.DeviceID,
				"port":         receipt.Port,
				"duration_ms":  in.Duration.Milliseconds(),
				"state":        receipt.State,
				"started_at":   receipt.StartedAt,
				"confirmed_at": receipt.ConfirmedAt,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        receipt.ConfirmedAt,
		})
	})
	if err != nil {
		log.Printf("accessdevice: command %s completed but audit failed: %v", receipt.CommandID, err)
		return false
	}
	return true
}

type GetAccessActuatorStatus struct {
	Actuator accessdevice.Actuator
}

func NewGetAccessActuatorStatus(actuator accessdevice.Actuator) *GetAccessActuatorStatus {
	return &GetAccessActuatorStatus{Actuator: actuator}
}

func (uc *GetAccessActuatorStatus) Execute(ctx context.Context) accessdevice.DeviceStatus {
	if uc.Actuator == nil {
		return accessdevice.DeviceStatus{Detail: "no hay un adaptador configurado"}
	}
	return uc.Actuator.Status(ctx)
}
