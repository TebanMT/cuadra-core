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

type requestDeletionExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.RequestAccountDeletionInput) (personalApp.Acknowledgement, error)
}

type DeletionController struct{ request requestDeletionExecutor }

func NewDeletionController(request requestDeletionExecutor) (*DeletionController, error) {
	if request == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &DeletionController{request}, nil
}

func (h *DeletionController) Request(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	for name := range c.Request.Header {
		if strings.EqualFold(name, "X-Attempt-Secret") || strings.EqualFold(name, "X-Result-Secret") {
			writeAuthError(c, id, personalErrors.ErrInvalidDeletion)
			return
		}
	}
	var body struct {
		Confirmation  string `json:"confirmation"`
		PolicyVersion string `json:"policy_version"`
		ResultSecret  string `json:"result_secret"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/deletion", &body)
	if !ok {
		return
	}
	proof, ok := readDeviceProof(c)
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidProof)
		return
	}
	result, err := h.request.Execute(c.Request.Context(), actor, personalApp.RequestAccountDeletionInput{OperationID: op, RequestHash: hash, Confirmation: body.Confirmation, PolicyVersion: body.PolicyVersion, ResultSecret: body.ResultSecret, ProofToken: proof})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
