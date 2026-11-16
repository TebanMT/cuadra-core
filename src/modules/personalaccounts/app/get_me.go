package app

import (
	"context"
	"strconv"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/me"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

type MeProfile struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	WeightUnit  string `json:"weight_unit"`
	Revision    string `json:"revision"`
}
type MeCapabilities = protocol.Capabilities

type MeResult struct {
	Profile          MeProfile      `json:"profile"`
	PhoneMasked      string         `json:"phone_masked"`
	Status           string         `json:"status"`
	SecurityVersion  string         `json:"security_version"`
	RecoveryPrepared bool           `json:"recovery_prepared"`
	Capabilities     MeCapabilities `json:"capabilities"`
}
type GetMeDependencies struct {
	UoW          shared.UnitOfWork
	Sessions     repository.AuthSessionRepository
	Me           repository.MeRepository
	Capabilities me.Capabilities
}
type GetMe struct{ dependencies GetMeDependencies }

func NewGetMe(d GetMeDependencies) (*GetMe, error) {
	if d.UoW == nil || d.Sessions == nil || d.Me == nil || d.Capabilities.Validate() != nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	d.Capabilities = d.Capabilities.Clone()
	return &GetMe{dependencies: d}, nil
}

// Execute receives claims from the verified middleware, never from a body. A
// valid signature alone is insufficient: current session state is checked here.
func (u *GetMe) Execute(ctx context.Context, actor security.AccessClaims) (MeResult, error) {
	if !validPersonalActor(actor) {
		return MeResult{}, personalErrors.ErrInvalidAccessToken
	}
	d := u.dependencies
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return MeResult{}, err
	}
	tx, err := d.UoW.Query(ctx)
	if err != nil {
		return MeResult{}, err
	}
	defer shared.CloseQuery(tx)
	if err = requirePersonalSession(d.Sessions, tx, actor); err != nil {
		return MeResult{}, err
	}
	snapshot, err := d.Me.Get(tx, actor.AccountID, actor.SessionID, actor.DeviceID)
	if err != nil {
		return MeResult{}, err
	}
	if snapshot.Validate() != nil || snapshot.Profile.ID != actor.AccountID {
		return MeResult{}, personalErrors.ErrInvalidMe
	}
	c := d.Capabilities.Clone()
	return MeResult{Profile: MeProfile{ID: snapshot.Profile.ID.String(), DisplayName: snapshot.Profile.DisplayName, WeightUnit: string(snapshot.Profile.WeightUnit), Revision: strconv.FormatInt(snapshot.Profile.Revision, 10)}, PhoneMasked: snapshot.PhoneMasked, Status: string(snapshot.Profile.Status), SecurityVersion: strconv.FormatInt(snapshot.SecurityVersion, 10), RecoveryPrepared: snapshot.RecoveryPrepared, Capabilities: MeCapabilities{ProtocolVersion: c.ProtocolVersion, MinimumProtocolVersion: c.MinimumProtocolVersion, MinimumAppVersion: c.MinimumAppVersion, MaxPushOperations: c.MaxPushOperations, MaxRequestBytes: c.MaxRequestBytes, MaxGroupBytes: c.MaxGroupBytes, Features: c.Features, PolicyVersion: c.PolicyVersion}}, nil
}

func validPersonalActor(actor security.AccessClaims) bool {
	for _, id := range []uuid.UUID{actor.AccountID, actor.SessionID, actor.DeviceID} {
		if id == uuid.Nil || id.Variant() != uuid.RFC4122 || id.Version() < 1 || id.Version() > 8 {
			return false
		}
	}
	return true
}
func requirePersonalSession(repo repository.AuthSessionRepository, tx shared.Transaction, actor security.AccessClaims) error {
	active, err := repo.SessionActive(tx, actor.AccountID, actor.SessionID, actor.DeviceID)
	if err != nil {
		return err
	}
	if !active {
		return personalErrors.ErrSessionRevoked
	}
	return nil
}
