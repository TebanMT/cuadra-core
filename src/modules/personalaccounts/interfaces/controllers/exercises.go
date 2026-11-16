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
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
)

type listExercisesExecutor interface {
	Execute(context.Context, security.AccessClaims, trainingApp.ListExercisesInput) (trainingApp.ExerciseListResult, error)
}
type ExercisesController struct{ list listExercisesExecutor }

func NewExercisesController(list listExercisesExecutor) (*ExercisesController, error) {
	if list == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &ExercisesController{list}, nil
}

func exerciseListQuery(raw string) (trainingApp.ListExercisesInput, error) {
	var in trainingApp.ListExercisesInput
	values, err := url.ParseQuery(raw)
	if err != nil {
		return in, trainingErrors.ErrInvalidExercise
	}
	for key, items := range values {
		if len(items) != 1 || items[0] == "" && key != "query" {
			return in, trainingErrors.ErrInvalidExercise
		}
		switch key {
		case "cursor":
			in.Cursor = items[0]
			if len(in.Cursor) > protocol.CursorMaximumLength {
				return in, trainingErrors.ErrInvalidExercise
			}
		case "limit":
			in.Limit, err = strconv.Atoi(items[0])
			if err != nil || in.Limit < 1 || in.Limit > 100 || strconv.Itoa(in.Limit) != items[0] {
				return in, trainingErrors.ErrInvalidExercise
			}
		case "query":
			in.Query = items[0]
		default:
			return in, trainingErrors.ErrInvalidExercise
		}
	}
	if _, err := exercise.NormalizeListFilter(in.Query); err != nil {
		return in, err
	}
	return in, nil
}

func writeExerciseListError(c *gin.Context, id string, err error) {
	switch {
	case errors.Is(err, protocol.ErrCursorExpired):
		writeAuthFault(c, id, 410, "CURSOR_EXPIRED", "Vuelve a cargar la lista de ejercicios desde el principio.", "retry", false)
	case errors.Is(err, protocol.ErrCursorEpochChanged):
		writeAuthFault(c, id, 410, "EPOCH_CHANGED", "La lista cambió. Vuelve a cargarla desde el principio.", "retry", false)
	case errors.Is(err, trainingErrors.ErrInvalidExercise), errors.Is(err, protocol.ErrCursorInvalid):
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa los filtros de búsqueda de ejercicios.", "correct", false)
	case errors.Is(err, protocol.ErrCommandActor):
		writeAuthError(c, id, personalErrors.ErrSessionRevoked)
	default:
		writeAuthFault(c, id, 503, "TEMPORARY_UNAVAILABLE", "No pudimos consultar tus ejercicios. Vuelve a intentar.", "retry", true)
	}
}

func (h *ExercisesController) List(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	invalid := c.Request.ContentLength != 0
	for name := range c.Request.Header {
		if strings.EqualFold(name, "X-Attempt-Secret") || strings.EqualFold(name, "X-Result-Secret") || strings.EqualFold(name, "X-Device-Proof") || strings.EqualFold(name, "Idempotency-Key") {
			invalid = true
		}
	}
	if invalid {
		writeExerciseListError(c, id, trainingErrors.ErrInvalidExercise)
		return
	}
	in, err := exerciseListQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeExerciseListError(c, id, err)
		return
	}
	out, err := h.list.Execute(c.Request.Context(), actor, in)
	if err != nil {
		writeExerciseListError(c, id, err)
		return
	}
	c.JSON(200, out)
}
