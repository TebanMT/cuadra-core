// Command access-smoke runs the Arduino actuator without requiring a Tinta
// login. It is a bench-test tool only; production access goes through the
// authenticated, owner-only sidecar endpoint.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/accessdevice"
	"github.com/cuadra/cuadra-core/src/modules/checkins/infraestructure/accessdevice/arduino"
)

func main() {
	port := flag.String("port", os.Getenv("TINTA_ACCESS_ARDUINO_PORT"), "puerto serial (vacío = autodetectar)")
	duration := flag.Duration("duration", time.Second, "duración del pulso")
	baud := flag.Int("baud", 115200, "baudrate serial")
	flag.Parse()

	actuator := arduino.NewSerialActuator(arduino.Config{PortName: *port, BaudRate: *baud})
	defer actuator.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	receipt, err := actuator.Pulse(ctx, accessdevice.PulseCommand{
		ID:       uuid.New(),
		Duration: *duration,
	})
	if err != nil {
		status := actuator.Status(context.Background())
		fmt.Fprintf(os.Stderr, "no se pudo completar la prueba: %v\n", err)
		if len(status.AvailablePorts) > 0 {
			fmt.Fprintf(os.Stderr, "puertos candidatos: %v\n", status.AvailablePorts)
		}
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(receipt, "", "  ")
	fmt.Println(string(out))
}
