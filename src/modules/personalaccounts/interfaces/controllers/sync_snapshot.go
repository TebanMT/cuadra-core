//go:build server

package controllers

import (
	"context"
	"errors"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"strconv"
)

type createSnapshotExecutor interface {
	Execute(context.Context, protocol.Actor, uuid.UUID) (protocol.Snapshot, error)
}
type getSnapshotPageExecutor interface {
	Execute(context.Context, protocol.Actor, uuid.UUID, int) (protocol.SnapshotPage, error)
}
type SyncSnapshotController struct {
	create createSnapshotExecutor
	page   getSnapshotPageExecutor
}

func NewSyncSnapshotController(create createSnapshotExecutor, page getSnapshotPageExecutor) (*SyncSnapshotController, error) {
	if create == nil || page == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &SyncSnapshotController{create: create, page: page}, nil
}
func writeSnapshotError(c *gin.Context, id string, err error) {
	if writeCursorError(c, id, err) {
		return
	}
	switch {
	case errors.Is(err, protocol.ErrSnapshotExpired):
		writeAuthFault(c, id, 410, "CURSOR_EXPIRED", "La copia venció. Descarga otra; tus pendientes se conservan.", "bootstrap", false)
	case errors.Is(err, protocol.ErrSnapshotMissing):
		writeAuthFault(c, id, 404, "NOT_FOUND", "No encontramos esa página de sincronización.", "bootstrap", false)
	case errors.Is(err, protocol.ErrSnapshotConflict):
		writeAuthFault(c, id, 409, "IDEMPOTENCY_MISMATCH", "Ese identificador de copia no está disponible.", "review", false)
	case errors.Is(err, protocol.ErrCommandActor):
		writeAuthError(c, id, personalErrors.ErrSessionRevoked)
	default:
		writeAuthFault(c, id, 503, "TEMPORARY_UNAVAILABLE", "No pudimos completar la copia. Tus pendientes se conservan.", "retry", true)
	}
}
func (h *SyncSnapshotController) Create(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var body struct {
		ProtocolVersion int `json:"protocol_version"`
	}
	op, _, ok := decodePersonalMutation(c, id, "/sync/snapshots", &body)
	if !ok {
		return
	}
	if body.ProtocolVersion != 1 {
		writeSnapshotError(c, id, protocol.ErrCommandInvalid)
		return
	}
	result, err := h.create.Execute(c.Request.Context(), protocol.Actor{AccountID: actor.AccountID, SessionID: actor.SessionID, DeviceID: actor.DeviceID}, op)
	if err != nil {
		writeSnapshotError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func (h *SyncSnapshotController) Page(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	snapshotID, err := canonicalUUID(c.Param("id"))
	number, e := strconv.Atoi(c.Param("number"))
	if err != nil || e != nil || number < 0 || number >= protocol.SnapshotMaxPages || strconv.Itoa(number) != c.Param("number") || c.Request.URL.RawQuery != "" {
		writeSnapshotError(c, id, protocol.ErrCommandInvalid)
		return
	}
	result, err := h.page.Execute(c.Request.Context(), protocol.Actor{AccountID: actor.AccountID, SessionID: actor.SessionID, DeviceID: actor.DeviceID}, snapshotID, number)
	if err != nil {
		writeSnapshotError(c, id, err)
		return
	}
	c.JSON(200, result)
}
