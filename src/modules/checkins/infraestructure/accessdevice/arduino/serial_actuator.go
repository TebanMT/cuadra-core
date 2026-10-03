// Package arduino implements Tinta's first physical access actuator: an
// Arduino Nano connected to the sidecar over USB serial.
package arduino

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.bug.st/serial"

	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/accessdevice"
)

const protocolVersion = "tinta-access-serial/1"

var errAckTimeout = errors.New("timeout esperando confirmación del Arduino")

type Config struct {
	PortName        string
	BaudRate        int
	BootDelay       time.Duration
	ReadyTimeout    time.Duration
	AckTimeout      time.Duration
	ReadTimeout     time.Duration
	CompletionGrace time.Duration
}

func (c Config) withDefaults() Config {
	if c.BaudRate == 0 {
		c.BaudRate = 115200
	}
	if c.BootDelay == 0 {
		c.BootDelay = 1800 * time.Millisecond
	}
	if c.ReadyTimeout == 0 {
		c.ReadyTimeout = 3 * time.Second
	}
	if c.AckTimeout == 0 {
		c.AckTimeout = time.Second
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 50 * time.Millisecond
	}
	if c.CompletionGrace == 0 {
		c.CompletionGrace = time.Second
	}
	return c
}

type serialPort interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Drain() error
	ResetInputBuffer() error
	SetReadTimeout(time.Duration) error
	Close() error
}

type portOpener func(string, *serial.Mode) (serialPort, error)
type portLister func() ([]string, error)

// SerialActuator owns one persistent port. Keeping it open matters on classic
// Nano boards because opening the USB serial port toggles DTR and may reset the
// bootloader; opening once per access would add latency and risk duplicate work.
type SerialActuator struct {
	cfg  Config
	open portOpener
	list portLister

	mu       sync.Mutex
	port     serialPort
	portName string
	rx       []byte
}

func NewSerialActuator(cfg Config) *SerialActuator {
	return newSerialActuator(cfg,
		func(name string, mode *serial.Mode) (serialPort, error) { return serial.Open(name, mode) },
		serial.GetPortsList,
	)
}

func newSerialActuator(cfg Config, open portOpener, list portLister) *SerialActuator {
	return &SerialActuator{cfg: cfg.withDefaults(), open: open, list: list}
}

func (a *SerialActuator) Pulse(ctx context.Context, cmd accessdevice.PulseCommand) (accessdevice.Receipt, error) {
	if cmd.ID == uuid.Nil {
		return accessdevice.Receipt{}, errors.New("command_id vacío")
	}
	if cmd.Duration < accessdevice.MinPulseDuration || cmd.Duration > accessdevice.MaxPulseDuration {
		return accessdevice.Receipt{}, accessdevice.ErrInvalidDuration
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	ctx, cancel := withTimeoutIfMissing(ctx, a.cfg.BootDelay+a.cfg.ReadyTimeout+cmd.Duration+a.cfg.CompletionGrace+time.Second)
	defer cancel()

	if err := a.ensureConnectedLocked(ctx); err != nil {
		return accessdevice.Receipt{}, err
	}
	// A lightweight probe catches a cable removed since the previous command.
	if err := a.pingLocked(ctx, a.cfg.AckTimeout); err != nil {
		a.closeLocked()
		return accessdevice.Receipt{}, fmt.Errorf("%w: %v", accessdevice.ErrUnavailable, err)
	}

	id := cmd.ID.String()
	request := fmt.Sprintf("PULSE %s %d\n", id, cmd.Duration.Milliseconds())
	if err := a.writeLineLocked(request); err != nil {
		a.closeLocked()
		return accessdevice.Receipt{}, fmt.Errorf("%w: no se pudo enviar el pulso: %v", accessdevice.ErrUnavailable, err)
	}

	state, at, err := a.awaitAckLocked(ctx, id, a.cfg.AckTimeout, "STARTED", "COMPLETED")
	if err != nil {
		a.closeLocked()
		return accessdevice.Receipt{}, fmt.Errorf("%w: %v", accessdevice.ErrUnavailable, err)
	}
	startedAt := at
	if state != "COMPLETED" {
		_, completedAt, err := a.awaitAckLocked(ctx, id, cmd.Duration+a.cfg.CompletionGrace, "COMPLETED")
		if err != nil {
			a.closeLocked()
			return accessdevice.Receipt{}, fmt.Errorf("%w: el Arduino aceptó el comando pero no confirmó su término: %v", accessdevice.ErrUnavailable, err)
		}
		at = completedAt
	}

	return accessdevice.Receipt{
		CommandID:   cmd.ID,
		DeviceID:    "arduino-nano:" + a.portName,
		Port:        a.portName,
		StartedAt:   startedAt,
		ConfirmedAt: at,
		State:       "completed",
	}, nil
}

func (a *SerialActuator) Status(ctx context.Context) accessdevice.DeviceStatus {
	a.mu.Lock()
	defer a.mu.Unlock()

	ports, listErr := a.availablePortsLocked()
	status := accessdevice.DeviceStatus{
		Configured:     strings.TrimSpace(a.cfg.PortName) != "" || len(ports) == 1,
		AvailablePorts: ports,
		Protocol:       protocolVersion,
	}
	if listErr != nil {
		// An explicitly configured COM/path can still be opened even when the
		// OS cannot enumerate ports (seen with restricted Windows accounts).
		if strings.TrimSpace(a.cfg.PortName) == "" {
			status.Detail = "no fue posible enumerar puertos seriales: " + listErr.Error()
			return status
		}
		status.AvailablePorts = []string{}
	}

	ctx, cancel := withTimeoutIfMissing(ctx, a.cfg.BootDelay+a.cfg.ReadyTimeout+a.cfg.AckTimeout+time.Second)
	defer cancel()
	if err := a.ensureConnectedLocked(ctx); err != nil {
		status.Detail = err.Error()
		return status
	}
	if err := a.pingLocked(ctx, a.cfg.AckTimeout); err != nil {
		a.closeLocked()
		status.Detail = fmt.Sprintf("Arduino sin respuesta: %v", err)
		return status
	}

	status.Configured = true
	status.Connected = true
	status.Port = a.portName
	status.DeviceID = "arduino-nano:" + a.portName
	status.Detail = "Arduino listo"
	return status
}

func (a *SerialActuator) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closeLocked()
}

func (a *SerialActuator) ensureConnectedLocked(ctx context.Context) error {
	if a.port != nil {
		return nil
	}

	name, err := a.resolvePortLocked()
	if err != nil {
		return fmt.Errorf("%w: %v", accessdevice.ErrUnavailable, err)
	}
	port, err := a.open(name, &serial.Mode{
		BaudRate: a.cfg.BaudRate,
		InitialStatusBits: &serial.ModemOutputBits{
			DTR: false,
			RTS: false,
		},
	})
	if err != nil {
		return fmt.Errorf("%w: no se pudo abrir %s: %v", accessdevice.ErrUnavailable, name, err)
	}
	if err := port.SetReadTimeout(a.cfg.ReadTimeout); err != nil {
		_ = port.Close()
		return fmt.Errorf("%w: no se pudo configurar el timeout serial: %v", accessdevice.ErrUnavailable, err)
	}
	_ = port.ResetInputBuffer()
	a.port = port
	a.portName = name
	a.rx = nil

	if err := waitContext(ctx, a.cfg.BootDelay); err != nil {
		a.closeLocked()
		return err
	}
	if err := a.waitReadyLocked(ctx); err != nil {
		a.closeLocked()
		return fmt.Errorf("%w: %s abrió pero el firmware Tinta no respondió: %v", accessdevice.ErrUnavailable, name, err)
	}
	return nil
}

func (a *SerialActuator) waitReadyLocked(ctx context.Context) error {
	deadline := time.Now().Add(a.cfg.ReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		attempt := a.cfg.AckTimeout
		if remaining := time.Until(deadline); remaining < attempt {
			attempt = remaining
		}
		if err := a.pingLocked(ctx, attempt); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	if lastErr == nil {
		lastErr = errAckTimeout
	}
	return lastErr
}

func (a *SerialActuator) pingLocked(ctx context.Context, timeout time.Duration) error {
	id := uuid.New().String()
	if err := a.writeLineLocked("PING " + id + "\n"); err != nil {
		return err
	}
	_, _, err := a.awaitAckLocked(ctx, id, timeout, "READY")
	return err
}

func (a *SerialActuator) awaitAckLocked(
	ctx context.Context,
	id string,
	timeout time.Duration,
	accepted ...string,
) (string, time.Time, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		line, err := a.readLineLocked(ctx, deadline)
		if err != nil {
			return "", time.Time{}, err
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != id {
			continue
		}
		if fields[0] == "ERR" {
			return "", time.Time{}, errors.New(strings.Join(fields[2:], " "))
		}
		if fields[0] != "ACK" {
			continue
		}
		for _, state := range accepted {
			if fields[2] == state {
				return state, time.Now().UTC(), nil
			}
		}
	}
	return "", time.Time{}, errAckTimeout
}

func (a *SerialActuator) readLineLocked(ctx context.Context, deadline time.Time) (string, error) {
	buf := make([]byte, 128)
	for {
		if i := bytes.IndexByte(a.rx, '\n'); i >= 0 {
			line := strings.TrimSpace(string(a.rx[:i]))
			a.rx = a.rx[i+1:]
			return line, nil
		}
		if len(a.rx) > 1024 {
			a.rx = nil
			return "", errors.New("respuesta serial demasiado larga")
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !time.Now().Before(deadline) {
			return "", errAckTimeout
		}
		n, err := a.port.Read(buf)
		if err != nil {
			return "", err
		}
		if n > 0 {
			a.rx = append(a.rx, buf[:n]...)
		}
	}
}

func (a *SerialActuator) writeLineLocked(line string) error {
	payload := []byte(line)
	for len(payload) > 0 {
		n, err := a.port.Write(payload)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("escritura serial vacía")
		}
		payload = payload[n:]
	}
	return a.port.Drain()
}

func (a *SerialActuator) resolvePortLocked() (string, error) {
	if name := strings.TrimSpace(a.cfg.PortName); name != "" {
		return name, nil
	}
	ports, err := a.availablePortsLocked()
	if err != nil {
		return "", err
	}
	switch len(ports) {
	case 0:
		return "", errors.New("no se detectó un Arduino; conecta el Nano o configura TINTA_ACCESS_ARDUINO_PORT")
	case 1:
		return ports[0], nil
	default:
		return "", fmt.Errorf("hay varios puertos candidatos (%s); configura TINTA_ACCESS_ARDUINO_PORT", strings.Join(ports, ", "))
	}
}

func (a *SerialActuator) availablePortsLocked() ([]string, error) {
	ports, err := a.list()
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, len(ports))
	for _, name := range ports {
		if isLikelyArduinoPort(name) {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	return candidates, nil
}

func isLikelyArduinoPort(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(lower, "com") {
		_, err := strconv.Atoi(strings.TrimPrefix(lower, "com"))
		return err == nil
	}
	for _, marker := range []string{"usbserial", "wchusbserial", "usbmodem", "ttyusb", "ttyacm"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (a *SerialActuator) closeLocked() error {
	if a.port == nil {
		return nil
	}
	err := a.port.Close()
	a.port = nil
	a.portName = ""
	a.rx = nil
	return err
}

func withTimeoutIfMissing(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func waitContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
