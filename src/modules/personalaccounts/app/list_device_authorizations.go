package app

import (
	"context"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
)

const deviceAuthorizationsCursorOrder = "created_at.desc-id.desc.v1"

type ListDeviceAuthorizationsInput struct {
	Cursor string
	Limit  int
}
type DeviceAuthorizationListResult struct {
	Items      []DeviceAuthorizationResult `json:"items"`
	NextCursor *string                     `json:"next_cursor"`
}
type ListDeviceAuthorizationsDependencies struct {
	UoW            shared.UnitOfWork
	Sessions       repository.AuthSessionRepository
	Authorizations repository.DeviceAuthorizationRepository
	Cursors        protocol.CursorSigner
	Now            func() time.Time
}
type ListDeviceAuthorizations struct {
	dependencies ListDeviceAuthorizationsDependencies
}

func NewListDeviceAuthorizations(d ListDeviceAuthorizationsDependencies) (*ListDeviceAuthorizations, error) {
	if d.UoW == nil || d.Sessions == nil || d.Authorizations == nil || d.Cursors == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &ListDeviceAuthorizations{dependencies: d}, nil
}
func (u *ListDeviceAuthorizations) Execute(ctx context.Context, actor security.AccessClaims, in ListDeviceAuthorizationsInput) (DeviceAuthorizationListResult, error) {
	if !validPersonalActor(actor) {
		return DeviceAuthorizationListResult{}, personalErrors.ErrInvalidAccessToken
	}
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 1 || in.Limit > 100 || len(in.Cursor) > protocol.CursorMaximumLength {
		return DeviceAuthorizationListResult{}, personalErrors.ErrInvalidDeviceAuthorization
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return DeviceAuthorizationListResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := u.dependencies
	result := DeviceAuthorizationListResult{Items: []DeviceAuthorizationResult{}}
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		epoch, err := d.Authorizations.StreamEpoch(tx, actor)
		if err != nil {
			return err
		}
		binding := protocol.CursorBinding{AccountID: actor.AccountID, Epoch: epoch, Query: protocol.CursorDeviceAuthorizations, Filters: map[string]string{"status": "pending"}, Order: deviceAuthorizationsCursorOrder}
		now := d.Now()
		if now.IsZero() || now.UTC().Year() < 1 || now.UTC().Year() > 9999 {
			return protocol.ErrCursorInvalid
		}
		var after *deviceauthorization.ListPosition
		if in.Cursor != "" {
			position, err := d.Cursors.Verify(in.Cursor, binding, now)
			if err != nil {
				return err
			}
			at, err := time.Parse(time.RFC3339Nano, position.Key)
			if err != nil || at.UTC().Format(time.RFC3339Nano) != position.Key {
				return protocol.ErrCursorInvalid
			}
			after = &deviceauthorization.ListPosition{CreatedAt: at, ID: position.ID}
			if after.Validate() != nil {
				return protocol.ErrCursorInvalid
			}
		}
		items, err := d.Authorizations.List(tx, actor, after, in.Limit+1)
		if err != nil {
			return err
		}
		if len(items) > in.Limit+1 {
			return personalErrors.ErrInvalidDeviceAuthorization
		}
		for _, item := range items {
			if item.Validate() != nil || item.AccountID != actor.AccountID || item.Status != deviceauthorization.Pending {
				return personalErrors.ErrInvalidDeviceAuthorization
			}
		}
		if len(items) > in.Limit {
			items = items[:in.Limit]
			last := items[len(items)-1]
			next, err := d.Cursors.Issue(binding, protocol.CursorPosition{Key: last.CreatedAt.UTC().Format(time.RFC3339Nano), ID: last.ID}, now)
			if err != nil {
				return err
			}
			result.NextCursor = &next
		}
		for _, item := range items {
			result.Items = append(result.Items, deviceAuthorizationResult(&item, now))
		}
		return nil
	})
	if err != nil {
		return DeviceAuthorizationListResult{}, err
	}
	return result, nil
}
