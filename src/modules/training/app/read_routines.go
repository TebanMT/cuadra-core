package app

import (
	"context"
	"strconv"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

type RoutineDTO struct {
	ID                      string  `json:"id"`
	Revision                string  `json:"revision"`
	CreatedAt               string  `json:"created_at"`
	UpdatedAt               string  `json:"updated_at"`
	DeletedAt               *string `json:"deleted_at"`
	Title                   string  `json:"title"`
	Status                  string  `json:"status"`
	CurrentVersionID        *string `json:"current_version_id"`
	DuplicatedFromVersionID *string `json:"duplicated_from_version_id"`
}
type RoutineDraftDTO struct {
	ID            string         `json:"id"`
	Revision      string         `json:"revision"`
	BaseVersionID *string        `json:"base_version_id"`
	Content       *routine.Draft `json:"content"`
	DeletedAt     *string        `json:"deleted_at"`
}
type RoutineReadDependencies struct {
	UoW          shared.UnitOfWork
	Commands     protocolRepository.Commands
	Routines     repository.RoutineRepository
	Publications repository.RoutinePublicationRepository
}
type GetRoutine struct{ d RoutineReadDependencies }
type GetRoutineDraft struct{ d RoutineReadDependencies }
type GetRoutineVersion struct{ d RoutineReadDependencies }

func NewGetRoutine(d RoutineReadDependencies) (*GetRoutine, error) {
	if d.UoW == nil || d.Commands == nil || d.Routines == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &GetRoutine{d}, nil
}
func NewGetRoutineDraft(d RoutineReadDependencies) (*GetRoutineDraft, error) {
	if d.UoW == nil || d.Commands == nil || d.Routines == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &GetRoutineDraft{d}, nil
}
func NewGetRoutineVersion(d RoutineReadDependencies) (*GetRoutineVersion, error) {
	if d.UoW == nil || d.Commands == nil || d.Publications == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &GetRoutineVersion{d}, nil
}

func routineRead(ctx context.Context, uow shared.UnitOfWork, commands protocolRepository.Commands, actor security.AccessClaims, fn func(shared.Transaction) error) error {
	if !protocol.ValidID(actor.AccountID) || !protocol.ValidID(actor.DeviceID) || !protocol.ValidID(actor.SessionID) {
		return protocol.ErrCommandActor
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Query is READ ONLY and cannot acquire the account mutex. The same short
	// transaction as other personal lists serializes this read with revocation,
	// publication and epoch changes, without advancing any journal sequence.
	return uow.Command(ctx, func(tx shared.Transaction) error {
		if err := commands.RequireActor(tx, protocol.Actor{AccountID: actor.AccountID, SessionID: actor.SessionID, DeviceID: actor.DeviceID}, actor.DeviceID); err != nil {
			return err
		}
		return fn(tx)
	})
}
func (u *GetRoutine) Execute(ctx context.Context, actor security.AccessClaims, id uuid.UUID) (RoutineDTO, error) {
	var out RoutineDTO
	if !protocol.ValidID(id) {
		return out, trainingErrors.ErrInvalidRoutine
	}
	err := routineRead(ctx, u.d.UoW, u.d.Commands, actor, func(tx shared.Transaction) error {
		r, err := u.d.Routines.GetByID(tx, actor.AccountID, id)
		if err != nil {
			return err
		}
		if r.Validate() != nil || r.AccountID != actor.AccountID || r.ID != id {
			return trainingErrors.ErrTrainingPersistence
		}
		out = routineView(r)
		return nil
	})
	if err != nil {
		return RoutineDTO{}, err
	}
	return out, nil
}
func (u *GetRoutineDraft) Execute(ctx context.Context, actor security.AccessClaims, id uuid.UUID) (RoutineDraftDTO, error) {
	var out RoutineDraftDTO
	if !protocol.ValidID(id) {
		return out, trainingErrors.ErrInvalidRoutine
	}
	err := routineRead(ctx, u.d.UoW, u.d.Commands, actor, func(tx shared.Transaction) error {
		d, err := u.d.Routines.GetDraftByID(tx, actor.AccountID, id)
		if err != nil {
			return err
		}
		if d.Validate() != nil || d.AccountID != actor.AccountID || d.RoutineID != id {
			return trainingErrors.ErrTrainingPersistence
		}
		out = RoutineDraftDTO{ID: d.RoutineID.String(), Revision: strconv.FormatInt(d.Revision, 10), BaseVersionID: routineOptionalID(d.BaseVersionID), DeletedAt: routineOptionalTime(d.DeletedAt)}
		if d.Content != nil {
			content := d.Content.Clone()
			out.Content = &content
		}
		return nil
	})
	if err != nil {
		return RoutineDraftDTO{}, err
	}
	return out, nil
}
func (u *GetRoutineVersion) Execute(ctx context.Context, actor security.AccessClaims, id uuid.UUID) (protocol.RoutineVersionDTO, error) {
	var out protocol.RoutineVersionDTO
	if !protocol.ValidID(id) {
		return out, trainingErrors.ErrInvalidRoutine
	}
	err := routineRead(ctx, u.d.UoW, u.d.Commands, actor, func(tx shared.Transaction) error {
		v, err := u.d.Publications.GetVersion(tx, actor.AccountID, id)
		if err != nil {
			return err
		}
		if v.Validate() != nil || v.AccountID != actor.AccountID || v.ID != id {
			return trainingErrors.ErrTrainingPersistence
		}
		out = v.DTO()
		return nil
	})
	if err != nil {
		return protocol.RoutineVersionDTO{}, err
	}
	return out, nil
}
func routineView(r *routine.Routine) RoutineDTO {
	return RoutineDTO{ID: r.ID.String(), Revision: strconv.FormatInt(r.Revision, 10), CreatedAt: protocol.Timestamp(r.CreatedAt), UpdatedAt: protocol.Timestamp(r.UpdatedAt), DeletedAt: routineOptionalTime(r.DeletedAt), Title: r.Title, Status: r.Status, CurrentVersionID: routineOptionalID(r.CurrentVersionID), DuplicatedFromVersionID: routineOptionalID(r.DuplicatedFromVersionID)}
}
func routineOptionalID(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	text := id.String()
	return &text
}
func routineOptionalTime(at *time.Time) *string {
	if at == nil {
		return nil
	}
	text := protocol.Timestamp(*at)
	return &text
}
