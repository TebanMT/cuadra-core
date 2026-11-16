package app

import (
	"context"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
)

const devicesCursorOrder = "authorized_at.desc-id.desc.v1"

type ListDevicesInput struct {
	Cursor string
	Limit  int
}
type DeviceListItem struct {
	ID           string  `json:"id"`
	Platform     string  `json:"platform"`
	Label        string  `json:"label"`
	AuthorizedAt string  `json:"authorized_at"`
	RevokedAt    *string `json:"revoked_at"`
	LastSeenAt   *string `json:"last_seen_at"`
	Current      bool    `json:"current"`
}
type DeviceListResult struct {
	Items      []DeviceListItem `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}
type ListDevicesDependencies struct {
	UoW      shared.UnitOfWork
	Sessions repository.AuthSessionRepository
	Devices  repository.DevicesRepository
	Cursors  protocol.CursorSigner
	Now      func() time.Time
}
type ListDevices struct{ dependencies ListDevicesDependencies }

func NewListDevices(d ListDevicesDependencies) (*ListDevices, error) {
	if d.UoW == nil || d.Sessions == nil || d.Devices == nil || d.Cursors == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &ListDevices{dependencies: d}, nil
}

func (u *ListDevices) Execute(ctx context.Context, actor security.AccessClaims, in ListDevicesInput) (DeviceListResult, error) {
	if !validPersonalActor(actor) {
		return DeviceListResult{}, personalErrors.ErrInvalidAccessToken
	}
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 1 || in.Limit > 100 || len(in.Cursor) > protocol.CursorMaximumLength {
		return DeviceListResult{}, personalErrors.ErrInvalidDevice
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return DeviceListResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := u.dependencies
	result := DeviceListResult{Items: []DeviceListItem{}}
	// The read deliberately shares the stream mutex with revocation. Command
	// creates a short transaction; this callback makes no business writes.
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		epoch, err := d.Devices.StreamEpoch(tx, actor)
		if err != nil {
			return err
		}
		now := d.Now()
		if now.IsZero() || now.UTC().Year() < 1 || now.UTC().Year() > 9999 {
			return protocol.ErrCursorInvalid
		}
		binding := protocol.CursorBinding{AccountID: actor.AccountID, Epoch: epoch, Query: protocol.CursorDevices, Order: devicesCursorOrder}
		var after *device.ListPosition
		if in.Cursor != "" {
			position, err := d.Cursors.Verify(in.Cursor, binding, now)
			if err != nil {
				return err
			}
			at, err := time.Parse(time.RFC3339Nano, position.Key)
			if err != nil || at.UTC().Format(time.RFC3339Nano) != position.Key {
				return protocol.ErrCursorInvalid
			}
			after = &device.ListPosition{AuthorizedAt: at, ID: position.ID}
			if after.Validate() != nil {
				return protocol.ErrCursorInvalid
			}
		}
		items, err := d.Devices.List(tx, actor, after, in.Limit+1)
		if err != nil {
			return err
		}
		if len(items) > in.Limit+1 {
			return personalErrors.ErrInvalidDevice
		}
		for _, item := range items {
			if item.Validate() != nil || item.AccountID != actor.AccountID {
				return personalErrors.ErrInvalidDevice
			}
		}
		if len(items) > in.Limit {
			items = items[:in.Limit]
			last := items[len(items)-1]
			// The opaque position retains PostgreSQL microseconds. Formatting the
			// key from the millisecond display DTO would omit or repeat rows.
			next, err := d.Cursors.Issue(binding, protocol.CursorPosition{ID: last.ID, Key: last.AuthorizedAt.UTC().Format(time.RFC3339Nano)}, now)
			if err != nil {
				return err
			}
			result.NextCursor = &next
		}
		for _, item := range items {
			view := DeviceListItem{ID: item.ID.String(), Platform: item.Platform, Label: item.Label, AuthorizedAt: accessTimestamp(item.AuthorizedAt), Current: item.ID == actor.DeviceID}
			if item.RevokedAt != nil {
				value := accessTimestamp(*item.RevokedAt)
				view.RevokedAt = &value
			}
			if item.LastSeenAt != nil {
				value := accessTimestamp(*item.LastSeenAt)
				view.LastSeenAt = &value
			}
			result.Items = append(result.Items, view)
		}
		return nil
	})
	if err != nil {
		return DeviceListResult{}, err
	}
	return result, nil
}
