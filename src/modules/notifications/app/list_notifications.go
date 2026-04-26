package app

import (
	"context"

	"github.com/google/uuid"

	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListNotificationsInput backs GET /api/v1/notifications. Status filter is
// one of "" / "pending" / "sent" / "failed" / "cancelled".
type ListNotificationsInput struct {
	GymID    uuid.UUID
	Status   string
	Page     int
	PageSize int
}

type ListNotificationsOutput struct {
	Items    []*notiDomain.Notification
	Total    int
	Page     int
	PageSize int
}

type ListNotifications struct {
	Notifications notiRepo.NotificationRepository
	UoW           sharedDomain.UnitOfWork
}

func NewListNotifications(notifications notiRepo.NotificationRepository, uow sharedDomain.UnitOfWork) *ListNotifications {
	return &ListNotifications{Notifications: notifications, UoW: uow}
}

func (uc *ListNotifications) Execute(ctx context.Context, in ListNotificationsInput) (*ListNotificationsOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	page := in.Page
	if page <= 0 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	rows, total, err := uc.Notifications.ListByGym(tx, in.GymID, in.Status, page, pageSize)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &ListNotificationsOutput{Items: rows, Total: total, Page: page, PageSize: pageSize}, nil
}
