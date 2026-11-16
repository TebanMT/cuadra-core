package app

import (
	"context"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deletion"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type GetDeletionResultInput struct {
	OperationID  uuid.UUID
	ResultSecret string
}

func (GetDeletionResultInput) String() string {
	return "account deletion result request (secret redacted)"
}
func (r GetDeletionResultInput) GoString() string { return r.String() }

type GetDeletionResultDependencies struct {
	UoW      shared.UnitOfWork
	Resolver repository.DeletionResultResolver
	Deletion repository.DeletionRepository
}
type GetDeletionResult struct{ dependencies GetDeletionResultDependencies }

func NewGetDeletionResult(d GetDeletionResultDependencies) (*GetDeletionResult, error) {
	if d.UoW == nil || d.Resolver == nil || d.Deletion == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &GetDeletionResult{d}, nil
}
func (u *GetDeletionResult) Execute(ctx context.Context, in GetDeletionResultInput) (Acknowledgement, error) {
	if err := shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return Acknowledgement{}, err
	}
	hash, err := deletion.ResultHash(in.OperationID, in.ResultSecret)
	if err != nil {
		return Acknowledgement{}, personalErrors.ErrDeletionResultNotFound
	}
	d := u.dependencies
	id, err := d.Resolver.Resolve(ctx, hash)
	if err != nil {
		return Acknowledgement{}, err
	}
	ctx, err = shared.WithAuthenticatedPersonalAccount(ctx, id)
	if err != nil {
		return Acknowledgement{}, err
	}
	tx, err := d.UoW.Query(ctx)
	if err != nil {
		return Acknowledgement{}, err
	}
	defer shared.CloseQuery(tx)
	// This capability intentionally survives withdrawal of all sessions. It
	// grants only the acknowledgement, never a session or account read model.
	r, err := d.Deletion.GetResult(tx, id, in.OperationID, hash)
	if err != nil {
		return Acknowledgement{}, err
	}
	if r.OperationID != in.OperationID || r.Validate() != nil {
		return Acknowledgement{}, personalErrors.ErrDeletionResultNotFound
	}
	return deletionAcknowledgement(r), nil
}
