//go:build server

package controllers

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
)

type createPersonalExerciseExecutor interface {
	Execute(context.Context, security.AccessClaims, exercise.CreateCommand) (protocol.OperationResult, error)
}
type createRoutineExecutor interface {
	Execute(context.Context, security.AccessClaims, routine.CreateCommand) (protocol.OperationResult, error)
}
type saveRoutineDraftExecutor interface {
	Execute(context.Context, security.AccessClaims, routine.SaveDraftCommand) (protocol.OperationResult, error)
}
type publishRoutineExecutor interface {
	Execute(context.Context, security.AccessClaims, routine.PublishCommand) (protocol.OperationResult, error)
}

type CommandDependencies struct {
	Profile  updateProfileExecutor
	Exercise createPersonalExerciseExecutor
	Routine  createRoutineExecutor
	Draft    saveRoutineDraftExecutor
	Publish  publishRoutineExecutor
}

// Each closed variant dispatches to one use case and one personal transaction.
// Unknown command types never fall back to another aggregate or handler.
type CommandController struct {
	profile  updateProfileExecutor
	exercise createPersonalExerciseExecutor
	routine  createRoutineExecutor
	draft    saveRoutineDraftExecutor
	publish  publishRoutineExecutor
}

func NewCommandControllerWithDependencies(d CommandDependencies) (*CommandController, error) {
	if d.Profile == nil || d.Exercise == nil || d.Routine == nil || d.Draft == nil || d.Publish == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &CommandController{profile: d.Profile, exercise: d.Exercise, routine: d.Routine, draft: d.Draft, publish: d.Publish}, nil
}

func NewCommandControllerWithRoutine(profile updateProfileExecutor, exercise createPersonalExerciseExecutor, routine createRoutineExecutor) (*CommandController, error) {
	h, err := NewCommandController(profile, exercise)
	if err != nil || routine == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	h.routine = routine
	return h, nil
}

func NewCommandController(profile updateProfileExecutor, exercise createPersonalExerciseExecutor) (*CommandController, error) {
	if profile == nil || exercise == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &CommandController{profile: profile, exercise: exercise}, nil
}

type commandEnvelope struct {
	OperationID    json.RawMessage `json:"operation_id"`
	OriginDeviceID json.RawMessage `json:"origin_device_id"`
	Type           string          `json:"type"`
	EntityID       json.RawMessage `json:"entity_id"`
	Base           json.RawMessage `json:"base"`
	CapturedAt     json.RawMessage `json:"captured_at"`
	Payload        json.RawMessage `json:"payload"`
}

func (h *CommandController) Execute(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var envelope commandEnvelope
	op, _, ok := decodePersonalMutationBounded(c, id, "/commands", &envelope, 2097152)
	if !ok {
		return
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		writeCommandError(c, id, protocol.ErrCommandInvalid)
		return
	}
	var result protocol.OperationResult
	switch envelope.Type {
	case "profile.update":
		var command account.ProfileCommand
		if decodeClosedJSON(raw, &command) != nil || command.OperationID != op.String() || command.ValidateEnvelope() != nil || account.ValidateProfile(command.Payload.DisplayName, command.Payload.WeightUnit) != nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
		result, err = h.profile.Execute(c.Request.Context(), actor, command)
	case exercise.CreateCommandType:
		var command exercise.CreateCommand
		if decodeClosedJSON(raw, &command) != nil || command.OperationID != op.String() || command.ValidateEnvelope() != nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
		result, err = h.exercise.Execute(c.Request.Context(), actor, command)
	case routine.CreateCommandType:
		if h.routine == nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
		var command routine.CreateCommand
		if err = decodeClosedJSON(raw, &command); err != nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
		if command.OperationID != op.String() || command.ValidateEnvelope() != nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
		result, err = h.routine.Execute(c.Request.Context(), actor, command)
	case routine.SaveDraftCommandType:
		if h.draft == nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
		var command routine.SaveDraftCommand
		if decodeClosedJSON(raw, &command) != nil || command.OperationID != op.String() || command.ValidateEnvelope() != nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
		result, err = h.draft.Execute(c.Request.Context(), actor, command)
	case routine.PublishCommandType:
		if h.publish == nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
		var command routine.PublishCommand
		if decodeClosedJSON(raw, &command) != nil || command.OperationID != op.String() || command.ValidateEnvelope() != nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
		result, err = h.publish.Execute(c.Request.Context(), actor, command)
	default:
		writeCommandError(c, id, protocol.ErrCommandInvalid)
		return
	}
	if err != nil {
		writeCommandError(c, id, err)
		return
	}
	c.JSON(200, result)
}

func writeCommandError(c *gin.Context, id string, err error) {
	switch {
	case errors.Is(err, protocol.ErrOperationMismatch):
		writeAuthFault(c, id, 409, "IDEMPOTENCY_MISMATCH", "La operación cambió. Conserva el cambio original para reintentar.", "review", false)
	case errors.Is(err, protocol.ErrCommandActor):
		writeAuthError(c, id, personalErrors.ErrSessionRevoked)
	case errors.Is(err, protocol.ErrCommandInvalid), errors.Is(err, protocol.ErrCommandOrigin):
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa los datos del cambio.", "correct", false)
	case errors.Is(err, trainingErrors.ErrTrainingLimit), errors.Is(err, protocol.ErrCommandLimit):
		writeAuthFault(c, id, 413, "LIMIT_EXCEEDED", "La solicitud es demasiado grande. Conserva la captura y reduce su contenido.", "correct", false)
	default:
		writeAuthFault(c, id, 503, "TEMPORARY_UNAVAILABLE", "No pudimos confirmar el cambio. Conservamos la operación para reintentar.", "retry", true)
	}
}
