package arduino

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.bug.st/serial"

	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/accessdevice"
)

type scriptedPort struct {
	rx     bytes.Buffer
	closed bool
}

func (p *scriptedPort) Read(dst []byte) (int, error) {
	if p.closed {
		return 0, errors.New("closed")
	}
	return p.rx.Read(dst)
}

func (p *scriptedPort) Write(src []byte) (int, error) {
	line := strings.TrimSpace(string(src))
	fields := strings.Fields(line)
	if len(fields) >= 2 && fields[0] == "PING" {
		p.rx.WriteString("READY tinta-access-serial/1\r\n")
		p.rx.WriteString("ACK " + fields[1] + " READY\r\n")
	}
	if len(fields) >= 3 && fields[0] == "PULSE" {
		p.rx.WriteString("ACK ignored-command STARTED\n")
		p.rx.WriteString("ACK " + fields[1] + " STARTED\n")
		p.rx.WriteString("ACK " + fields[1] + " COMPLETED\n")
	}
	return len(src), nil
}

func (p *scriptedPort) Drain() error                       { return nil }
func (p *scriptedPort) ResetInputBuffer() error            { return nil }
func (p *scriptedPort) SetReadTimeout(time.Duration) error { return nil }
func (p *scriptedPort) Close() error {
	p.closed = true
	return nil
}

func testConfig() Config {
	return Config{
		BootDelay:       time.Nanosecond,
		ReadyTimeout:    100 * time.Millisecond,
		AckTimeout:      50 * time.Millisecond,
		ReadTimeout:     time.Millisecond,
		CompletionGrace: 50 * time.Millisecond,
	}
}

func TestSerialActuatorPulseReturnsCompletedReceipt(t *testing.T) {
	port := &scriptedPort{}
	actuator := newSerialActuator(testConfig(),
		func(name string, _ *serial.Mode) (serialPort, error) {
			if name != "/dev/cu.usbserial-test" {
				t.Fatalf("unexpected port: %s", name)
			}
			return port, nil
		},
		func() ([]string, error) {
			return []string{"/dev/cu.Bluetooth-Incoming-Port", "/dev/cu.usbserial-test"}, nil
		},
	)

	id := uuid.New()
	receipt, err := actuator.Pulse(context.Background(), accessdevice.PulseCommand{
		ID:       id,
		Duration: time.Second,
	})
	if err != nil {
		t.Fatalf("Pulse: %v", err)
	}
	if receipt.CommandID != id || receipt.State != "completed" {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	if receipt.Port != "/dev/cu.usbserial-test" {
		t.Fatalf("port = %q", receipt.Port)
	}
	if receipt.StartedAt.IsZero() || receipt.ConfirmedAt.IsZero() {
		t.Fatalf("timestamps must be populated: %+v", receipt)
	}
}

func TestSerialActuatorStatusRequiresExplicitPortWhenSeveralExist(t *testing.T) {
	actuator := newSerialActuator(testConfig(),
		func(string, *serial.Mode) (serialPort, error) {
			t.Fatal("port must not be opened when selection is ambiguous")
			return nil, nil
		},
		func() ([]string, error) {
			return []string{"/dev/cu.usbmodem-one", "/dev/cu.usbserial-two"}, nil
		},
	)

	status := actuator.Status(context.Background())
	if status.Connected || status.Configured {
		t.Fatalf("unexpected status: %+v", status)
	}
	if !strings.Contains(status.Detail, "TINTA_ACCESS_ARDUINO_PORT") {
		t.Fatalf("detail should explain explicit configuration: %q", status.Detail)
	}
}

func TestSerialActuatorRejectsUnsafePulseDuration(t *testing.T) {
	actuator := newSerialActuator(testConfig(), nil, nil)
	_, err := actuator.Pulse(context.Background(), accessdevice.PulseCommand{
		ID:       uuid.New(),
		Duration: 10 * time.Second,
	})
	if !errors.Is(err, accessdevice.ErrInvalidDuration) {
		t.Fatalf("expected ErrInvalidDuration, got %v", err)
	}
}

func TestLikelyArduinoPort(t *testing.T) {
	tests := map[string]bool{
		"/dev/cu.usbserial-1410":          true,
		"/dev/cu.wchusbserial-110":        true,
		"/dev/cu.usbmodem2101":            true,
		"/dev/ttyUSB0":                    true,
		"/dev/ttyACM0":                    true,
		"COM4":                            true,
		"/dev/cu.Bluetooth-Incoming-Port": false,
		"/dev/cu.debug-console":           false,
	}
	for name, want := range tests {
		if got := isLikelyArduinoPort(name); got != want {
			t.Errorf("isLikelyArduinoPort(%q) = %v, want %v", name, got, want)
		}
	}
}
