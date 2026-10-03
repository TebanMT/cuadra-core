package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/accessdevice"
)

type fakeActuator struct {
	receipt accessdevice.Receipt
	err     error
}

func (f *fakeActuator) Pulse(_ context.Context, cmd accessdevice.PulseCommand) (accessdevice.Receipt, error) {
	if f.err != nil {
		return accessdevice.Receipt{}, f.err
	}
	r := f.receipt
	r.CommandID = cmd.ID
	return r, nil
}

func (f *fakeActuator) Status(context.Context) accessdevice.DeviceStatus {
	return accessdevice.DeviceStatus{}
}
func (f *fakeActuator) Close() error { return nil }

func TestAccessActuatorReturnsConfirmedReceiptWithoutInventingAudit(t *testing.T) {
	now := time.Now().UTC()
	actuator := &fakeActuator{receipt: accessdevice.Receipt{
		DeviceID:    "arduino-nano:test",
		Port:        "test",
		StartedAt:   now,
		ConfirmedAt: now.Add(time.Second),
		State:       "completed",
	}}
	uc := NewTestAccessActuator(actuator, nil, nil)
	out, err := uc.Execute(context.Background(), TestAccessActuatorInput{
		GymID: uuid.New(), OwnerID: uuid.New(), Duration: time.Second,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Receipt.State != "completed" || out.Receipt.CommandID == uuid.Nil {
		t.Fatalf("unexpected output: %+v", out)
	}
	if out.AuditRecorded {
		t.Fatal("audit must be false when no recorder is wired")
	}
}

func TestAccessActuatorMapsDeviceFailureToBusinessError(t *testing.T) {
	uc := NewTestAccessActuator(&fakeActuator{err: errors.New("cable desconectado")}, nil, nil)
	_, err := uc.Execute(context.Background(), TestAccessActuatorInput{
		GymID: uuid.New(), OwnerID: uuid.New(), Duration: time.Second,
	})
	if err == nil || !errors.Is(err, accessdevice.ErrUnavailable) {
		t.Fatalf("expected unavailable error, got %v", err)
	}
}

func TestAccessActuatorRejectsDurationBeforeCallingHardware(t *testing.T) {
	uc := NewTestAccessActuator(&fakeActuator{}, nil, nil)
	_, err := uc.Execute(context.Background(), TestAccessActuatorInput{
		GymID: uuid.New(), OwnerID: uuid.New(), Duration: 50 * time.Millisecond,
	})
	if err == nil || !errors.Is(err, accessdevice.ErrInvalidDuration) {
		t.Fatalf("expected invalid duration, got %v", err)
	}
}
