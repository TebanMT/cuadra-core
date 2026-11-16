//go:build server

package controllers

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strings"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
)

type startProofExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.StartDeviceProofInput) (personalApp.DeviceProofResult, error)
}
type completeProofExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.CompleteDeviceProofInput) (personalApp.ProofTokenResult, error)
}
type DeviceProofController struct {
	start    startProofExecutor
	complete completeProofExecutor
}

func NewDeviceProofController(start startProofExecutor, complete completeProofExecutor) (*DeviceProofController, error) {
	if start == nil || complete == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &DeviceProofController{start: start, complete: complete}, nil
}
func (h *DeviceProofController) Start(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var body struct {
		ID          string                `json:"id"`
		Purpose     security.ProofPurpose `json:"purpose"`
		RequestHash string                `json:"request_hash"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/proofs", &body)
	if !ok {
		return
	}
	proofID, err := canonicalUUID(body.ID)
	target, decodeErr := hex.DecodeString(body.RequestHash)
	if err != nil || decodeErr != nil || len(target) != 32 || strings.ToLower(body.RequestHash) != body.RequestHash || !validProofPurpose(body.Purpose) {
		writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	result, err := h.start.Execute(c.Request.Context(), actor, personalApp.StartDeviceProofInput{OperationID: op, RequestHash: hash, ProofID: proofID, Purpose: body.Purpose, TargetRequestHash: [32]byte(target)})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func (h *DeviceProofController) Complete(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	proofID, err := canonicalUUID(c.Param("id"))
	if err != nil {
		writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	var body struct {
		Signature string `json:"signature"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/proofs/"+proofID.String()+"/complete", &body)
	if !ok {
		return
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(body.Signature)
	if err != nil || len(signature) != 64 || len(body.Signature) != 86 {
		writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	result, err := h.complete.Execute(c.Request.Context(), actor, personalApp.CompleteDeviceProofInput{OperationID: op, RequestHash: hash, ProofID: proofID, Signature: body.Signature})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func validProofPurpose(purpose security.ProofPurpose) bool {
	switch purpose {
	case security.AuthorizeDevice, security.RevokeDevices, security.RecoveryKey, security.PhoneChange, security.DeleteAccount, security.VaultUnlock:
		return true
	default:
		return false
	}
}
