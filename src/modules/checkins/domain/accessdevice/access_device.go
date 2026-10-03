// Package accessdevice defines the small domain seam between Tinta's access
// decisions and the physical device that carries them out. The first adapter
// is an Arduino Nano driving an LED; a relay/turnstile can implement the same
// contract later without leaking serial details into the check-in use cases.
package accessdevice

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	MinPulseDuration = 100 * time.Millisecond
	MaxPulseDuration = 5 * time.Second
)

var (
	ErrUnavailable     = errors.New("el actuador de acceso no está disponible")
	ErrInvalidDuration = errors.New("la duración del pulso debe estar entre 100 y 5000 ms")
)

// PulseCommand describes one bounded activation. A door relay should be
// pulsed, never left indefinitely in the active state by a cloud/UI command.
// ID is also the idempotency key understood by the device protocol.
type PulseCommand struct {
	ID       uuid.UUID
	Duration time.Duration
}

// Receipt is application-level confirmation from the device: the firmware
// accepted the command and returned COMPLETED after lowering the output pin.
// It does not yet prove that current flowed through the LED (or that a future
// door moved); that requires an independent physical feedback sensor.
type Receipt struct {
	CommandID   uuid.UUID
	DeviceID    string
	Port        string
	StartedAt   time.Time
	ConfirmedAt time.Time
	State       string
}

// DeviceStatus is intentionally operational rather than vendor-specific so
// the settings UI can eventually render Arduino, relay and access-controller
// adapters through one shape.
type DeviceStatus struct {
	Configured     bool
	Connected      bool
	DeviceID       string
	Port           string
	AvailablePorts []string
	Protocol       string
	Detail         string
}

// Actuator is the port owned by the checkins bounded context. Infrastructure
// adapters may use serial, HTTP or a vendor SDK behind it.
type Actuator interface {
	Pulse(context.Context, PulseCommand) (Receipt, error)
	Status(context.Context) DeviceStatus
	Close() error
}
