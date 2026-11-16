//go:build server

package controllers

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type listRoutinesExecutor interface {
	Execute(context.Context, security.AccessClaims, trainingApp.ListRoutinesInput) (trainingApp.RoutineListResult, error)
}
type getRoutineExecutor interface {
	Execute(context.Context, security.AccessClaims, uuid.UUID) (trainingApp.RoutineDTO, error)
}
type getRoutineDraftExecutor interface {
	Execute(context.Context, security.AccessClaims, uuid.UUID) (trainingApp.RoutineDraftDTO, error)
}
type getRoutineVersionExecutor interface {
	Execute(context.Context, security.AccessClaims, uuid.UUID) (protocol.RoutineVersionDTO, error)
}
type RoutinesController struct {
	list    listRoutinesExecutor
	get     getRoutineExecutor
	draft   getRoutineDraftExecutor
	version getRoutineVersionExecutor
}

func NewRoutinesController(list listRoutinesExecutor, get getRoutineExecutor, draft getRoutineDraftExecutor, version getRoutineVersionExecutor) (*RoutinesController, error) {
	if list == nil || get == nil || draft == nil || version == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &RoutinesController{list, get, draft, version}, nil
}
func routineListQuery(raw string) (trainingApp.ListRoutinesInput, error) {
	var in trainingApp.ListRoutinesInput
	values, err := url.ParseQuery(raw)
	if err != nil {
		return in, trainingErrors.ErrInvalidRoutine
	}
	for key, items := range values {
		if len(items) != 1 || (items[0] == "" && key != "query") {
			return in, trainingErrors.ErrInvalidRoutine
		}
		switch key {
		case "cursor":
			in.Cursor = items[0]
			if len(in.Cursor) > protocol.CursorMaximumLength {
				return in, trainingErrors.ErrInvalidRoutine
			}
		case "limit":
			in.Limit, err = strconv.Atoi(items[0])
			if err != nil || in.Limit < 1 || in.Limit > 100 || strconv.Itoa(in.Limit) != items[0] {
				return in, trainingErrors.ErrInvalidRoutine
			}
		case "status":
			in.Status = items[0]
		case "query":
			in.Query = items[0]
		default:
			return in, trainingErrors.ErrInvalidRoutine
		}
	}
	if _, err := routine.NormalizeListFilter(in.Status, in.Query); err != nil {
		return in, err
	}
	return in, nil
}
func writeRoutineReadError(c *gin.Context, id string, err error) {
	switch {
	case errors.Is(err, protocol.ErrCursorExpired):
		writeAuthFault(c, id, 410, "CURSOR_EXPIRED", "Vuelve a cargar la lista de rutinas desde el principio.", "retry", false)
	case errors.Is(err, protocol.ErrCursorEpochChanged):
		writeAuthFault(c, id, 410, "EPOCH_CHANGED", "La lista cambió. Vuelve a cargarla desde el principio.", "retry", false)
	case errors.Is(err, trainingErrors.ErrInvalidRoutine), errors.Is(err, protocol.ErrCursorInvalid):
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa la rutina y los filtros de búsqueda.", "correct", false)
	case errors.Is(err, trainingErrors.ErrRoutineNotFound):
		writeAuthFault(c, id, 404, "NOT_FOUND", "No encontramos esa rutina.", "review", false)
	case errors.Is(err, protocol.ErrCommandActor):
		writeAuthError(c, id, personalErrors.ErrSessionRevoked)
	default:
		writeAuthFault(c, id, 503, "TEMPORARY_UNAVAILABLE", "No pudimos consultar tus rutinas. Vuelve a intentar.", "retry", true)
	}
}
func (h *RoutinesController) List(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	if !routineReadEnvelope(c, id) {
		return
	}
	in, err := routineListQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeRoutineReadError(c, id, err)
		return
	}
	out, err := h.list.Execute(c.Request.Context(), actor, in)
	if err != nil {
		writeRoutineReadError(c, id, err)
		return
	}
	c.JSON(200, out)
}
func routineReadEnvelope(c *gin.Context, requestID string) bool {
	invalid := c.Request.ContentLength != 0
	for name := range c.Request.Header {
		if strings.EqualFold(name, "X-Attempt-Secret") || strings.EqualFold(name, "X-Result-Secret") || strings.EqualFold(name, "X-Device-Proof") || strings.EqualFold(name, "Idempotency-Key") {
			invalid = true
		}
	}
	if invalid {
		writeRoutineReadError(c, requestID, trainingErrors.ErrInvalidRoutine)
		return false
	}
	return true
}
func routineReadParams(c *gin.Context, requestID string) (security.AccessClaims, uuid.UUID, bool) {
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAccessToken)
		return actor, uuid.Nil, false
	}
	if !routineReadEnvelope(c, requestID) {
		return actor, uuid.Nil, false
	}
	id, err := canonicalUUID(c.Param("id"))
	if err != nil || c.Request.URL.RawQuery != "" {
		writeRoutineReadError(c, requestID, trainingErrors.ErrInvalidRoutine)
		return actor, uuid.Nil, false
	}
	return actor, id, true
}
func (h *RoutinesController) Get(c *gin.Context) {
	requestID := beginPersonalResponse(c)
	actor, id, ok := routineReadParams(c, requestID)
	if !ok {
		return
	}
	out, err := h.get.Execute(c.Request.Context(), actor, id)
	if err != nil {
		writeRoutineReadError(c, requestID, err)
		return
	}
	c.JSON(200, out)
}
func (h *RoutinesController) Draft(c *gin.Context) {
	requestID := beginPersonalResponse(c)
	actor, id, ok := routineReadParams(c, requestID)
	if !ok {
		return
	}
	out, err := h.draft.Execute(c.Request.Context(), actor, id)
	if err != nil {
		writeRoutineReadError(c, requestID, err)
		return
	}
	c.JSON(200, out)
}
func (h *RoutinesController) Version(c *gin.Context) {
	requestID := beginPersonalResponse(c)
	actor, id, ok := routineReadParams(c, requestID)
	if !ok {
		return
	}
	out, err := h.version.Execute(c.Request.Context(), actor, id)
	if err != nil {
		writeRoutineReadError(c, requestID, err)
		return
	}
	c.JSON(200, out)
}
