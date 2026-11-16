//go:build server

package controllers

import (
	"context"
	"strings"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
)

type rotateRecoveryExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.RotateRecoveryKeyInput) (personalApp.RecoveryKit, error)
}
type RecoveryKeyController struct{ rotate rotateRecoveryExecutor }

func NewRecoveryKeyController(rotate rotateRecoveryExecutor) (*RecoveryKeyController, error) {
	if rotate == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &RecoveryKeyController{rotate: rotate}, nil
}
func (h *RecoveryKeyController) Rotate(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/recovery-key", &body)
	if !ok {
		return
	}
	keyID, err := canonicalUUID(body.ID)
	proof, ok := readDeviceProof(c)
	if err != nil || !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidProof)
		return
	}
	result, err := h.rotate.Execute(c.Request.Context(), actor, personalApp.RotateRecoveryKeyInput{OperationID: op, RequestHash: hash, KeyID: keyID, ProofToken: proof})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}

// The capability travels in one header, never a URL or duplicate alias.
func readDeviceProof(c *gin.Context) (string, bool) {
	values := []string{}
	for name, items := range c.Request.Header {
		if strings.EqualFold(name, "X-Device-Proof") {
			values = append(values, items...)
		}
	}
	if len(values) != 1 || !secretPattern.MatchString(values[0]) {
		return "", false
	}
	return values[0], true
}
