package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/accessdevice"
	"github.com/cuadra/cuadra-core/src/shared/auth"
)

type controllerActuator struct {
	pulses int
}

func (a *controllerActuator) Pulse(_ context.Context, command accessdevice.PulseCommand) (accessdevice.Receipt, error) {
	a.pulses++
	now := time.Now().UTC()
	return accessdevice.Receipt{
		CommandID: command.ID, DeviceID: "arduino-nano:test", Port: "test",
		StartedAt: now, ConfirmedAt: now.Add(command.Duration), State: "completed",
	}, nil
}

func (a *controllerActuator) Status(context.Context) accessdevice.DeviceStatus {
	return accessdevice.DeviceStatus{Configured: true, Connected: true, DeviceID: "arduino-nano:test"}
}

func (a *controllerActuator) Close() error { return nil }

func TestAccessDeviceRoutesAreOwnerOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("access-device-test-secret")
	actuator := &controllerActuator{}
	controller := NewAccessDeviceController(
		chkApp.NewTestAccessActuator(actuator, nil, nil),
		chkApp.NewGetAccessActuatorStatus(actuator),
		tokens,
	)
	router := gin.New()
	controller.RegisterRoutes(router)

	gymID := uuid.New()
	operatorToken, err := tokens.GenerateAccessToken(uuid.New(), gymID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/access/device/test-pulse", strings.NewReader(`{"duration_ms":1000}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+operatorToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("operator status = %d, body=%s", response.Code, response.Body.String())
	}
	if actuator.pulses != 0 {
		t.Fatal("operator must not reach physical actuator")
	}

	ownerToken, err := tokens.GenerateAccessToken(uuid.New(), gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/access/device/test-pulse", strings.NewReader(`{"duration_ms":1000}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+ownerToken)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("owner status = %d, body=%s", response.Code, response.Body.String())
	}
	if actuator.pulses != 1 {
		t.Fatalf("owner pulses = %d", actuator.pulses)
	}
}
