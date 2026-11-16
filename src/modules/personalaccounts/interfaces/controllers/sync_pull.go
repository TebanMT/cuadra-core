//go:build server

package controllers

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	syncApp "github.com/cuadra/cuadra-core/src/shared/sync/personal/app"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
)

type pullExecutor interface {
	Execute(context.Context, protocol.Actor, syncApp.PullInput) (protocol.PullResult, error)
}
type SyncPullController struct{ pull pullExecutor }

func NewSyncPullController(pull pullExecutor) (*SyncPullController, error) {
	if pull == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &SyncPullController{pull: pull}, nil
}

// A closed query prevents duplicate cursor/limit ambiguity in proxies or
// frameworks. An explicitly empty cursor is not a new list or bootstrap.
func personalPageQuery(raw string) (cursor string, limit int, err error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", 0, protocol.ErrCommandInvalid
	}
	limit = 50
	for key, items := range values {
		if len(items) != 1 || items[0] == "" {
			return "", 0, protocol.ErrCommandInvalid
		}
		switch key {
		case "cursor":
			cursor = items[0]
			if len(cursor) > protocol.CursorMaximumLength {
				return "", 0, protocol.ErrCommandInvalid
			}
		case "limit":
			limit, err = strconv.Atoi(items[0])
			if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != items[0] {
				return "", 0, protocol.ErrCommandInvalid
			}
		default:
			return "", 0, protocol.ErrCommandInvalid
		}
	}
	return cursor, limit, nil
}
func writeCursorError(c *gin.Context, id string, err error) bool {
	switch {
	case errors.Is(err, protocol.ErrCursorExpired):
		writeAuthFault(c, id, 410, "CURSOR_EXPIRED", "Vuelve a descargar una copia completa. Tus pendientes se conservan.", "bootstrap", false)
	case errors.Is(err, protocol.ErrCursorEpochChanged):
		writeAuthFault(c, id, 410, "EPOCH_CHANGED", "La copia del servidor cambió. Tus pendientes se conservan.", "bootstrap", false)
	case errors.Is(err, protocol.ErrCursorInvalid), errors.Is(err, protocol.ErrCommandInvalid):
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "La página solicitada no es válida.", "correct", false)
	default:
		return false
	}
	return true
}
func (h *SyncPullController) Get(c *gin.Context) {
	id := beginPersonalResponse(c)
	claims, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	cursor, limit, err := personalPageQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeCursorError(c, id, err)
		return
	}
	result, err := h.pull.Execute(c.Request.Context(), protocol.Actor{AccountID: claims.AccountID, SessionID: claims.SessionID, DeviceID: claims.DeviceID}, syncApp.PullInput{Cursor: cursor, Limit: limit})
	if err != nil {
		if writeCursorError(c, id, err) {
			return
		}
		if errors.Is(err, protocol.ErrCommandActor) {
			writeAuthError(c, id, personalErrors.ErrSessionRevoked)
			return
		}
		writeAuthFault(c, id, 503, "TEMPORARY_UNAVAILABLE", "No pudimos descargar los cambios. Tus pendientes se conservan.", "retry", true)
		return
	}
	c.JSON(200, result)
}
