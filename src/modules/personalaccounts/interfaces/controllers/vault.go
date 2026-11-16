//go:build server

package controllers

import (
	"context"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
)

type createVaultExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.CreatePersonalVaultInput) (personalApp.VaultResult, error)
}
type unlockVaultExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.UnlockPersonalVaultInput) (personalApp.VaultResult, error)
}
type VaultController struct {
	create createVaultExecutor
	unlock unlockVaultExecutor
}

func NewVaultController(create createVaultExecutor, unlock unlockVaultExecutor) (*VaultController, error) {
	if create == nil || unlock == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &VaultController{create: create, unlock: unlock}, nil
}
func (h *VaultController) Create(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/vaults", &body)
	if !ok {
		return
	}
	vaultID, err := canonicalUUID(body.ID)
	if err != nil {
		writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	result, err := h.create.Execute(c.Request.Context(), actor, personalApp.CreatePersonalVaultInput{OperationID: op, RequestHash: hash, VaultID: vaultID})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func (h *VaultController) Unlock(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	vaultID, err := canonicalUUID(c.Param("id"))
	if err != nil {
		writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	var body struct{}
	op, hash, ok := decodePersonalMutation(c, id, "/me/vaults/"+vaultID.String()+"/unlock", &body)
	if !ok {
		return
	}
	proof, ok := readDeviceProof(c)
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidProof)
		return
	}
	result, err := h.unlock.Execute(c.Request.Context(), actor, personalApp.UnlockPersonalVaultInput{OperationID: op, RequestHash: hash, VaultID: vaultID, ProofToken: proof})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
