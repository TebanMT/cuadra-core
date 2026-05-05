package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	alertDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/alertconfig"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// AlertConfigView is one row of the GET /owner-alerts payload. Mirrors the
// shape useNotifications.ts on the FE expects (key, enabled, description).
type AlertConfigView struct {
	Key         alertDomain.Key
	Enabled     bool
	Description string
}

// ListOwnerAlerts merges the canonical default library with the gym's
// overrides. UC-040 DA-40.1.
type ListOwnerAlerts struct {
	Configs notiRepo.AlertConfigRepository
	UoW     sharedDomain.UnitOfWork
}

func NewListOwnerAlerts(configs notiRepo.AlertConfigRepository, uow sharedDomain.UnitOfWork) *ListOwnerAlerts {
	return &ListOwnerAlerts{Configs: configs, UoW: uow}
}

func (uc *ListOwnerAlerts) Execute(ctx context.Context, gymID uuid.UUID) ([]AlertConfigView, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	overrides, err := uc.Configs.ListByGym(tx, gymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	byKey := make(map[alertDomain.Key]*alertDomain.Config, len(overrides))
	for _, o := range overrides {
		byKey[o.Key] = o
	}
	defaults := alertDomain.Defaults()
	out := make([]AlertConfigView, 0, len(defaults))
	for _, d := range defaults {
		view := AlertConfigView{
			Key:         d.Key,
			Description: d.Description,
			Enabled:     d.EnabledByDefault,
		}
		if o, ok := byKey[d.Key]; ok {
			view.Enabled = o.Enabled
		}
		out = append(out, view)
	}
	return out, nil
}

// UpdateOwnerAlertInput backs PATCH /api/v1/owner-alerts/:key. The key must
// be one of alertconfig.Defaults — unknown keys are rejected so a stale FE
// build can't quietly create dead rows.
type UpdateOwnerAlertInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Key         alertDomain.Key
	Enabled     bool
}

type UpdateOwnerAlert struct {
	Configs notiRepo.AlertConfigRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
}

func NewUpdateOwnerAlert(configs notiRepo.AlertConfigRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateOwnerAlert {
	return &UpdateOwnerAlert{Configs: configs, UoW: uow, Audit: recorder}
}

func (uc *UpdateOwnerAlert) Execute(ctx context.Context, in UpdateOwnerAlertInput) (*alertDomain.Config, error) {
	if alertDomain.LookupDefault(in.Key) == nil {
		return nil, sharedDomain.NewValidationError(notiErrors.ErrAlertKeyUnknown)
	}
	now := time.Now().UTC()
	var out *alertDomain.Config
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		existing, err := uc.Configs.GetByGymAndKey(tx, in.GymID, in.Key)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var cfg *alertDomain.Config
		if existing == nil {
			cfg, err = alertDomain.New(in.GymID, in.Key, in.Enabled, now)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
		} else {
			cfg = existing
			cfg.SetEnabled(in.Enabled, now)
		}
		saved, err := uc.Configs.Upsert(tx, cfg)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// Audit row keys off (gym_id, alert_key); since there's no surrogate
		// id, we use gym_id as the entity_id placeholder and stash the key
		// inside Changes.
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "owner_alert_configs",
			EntityID:    in.GymID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"alert_key": string(saved.Key),
				"enabled":   saved.Enabled,
				"version":   saved.Version,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = saved
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// IsOwnerAlertEnabled is a small query helper used by the dispatch path.
// Returns the per-gym override when present, otherwise the default flag.
// Errors propagate so callers don't accidentally silence a DB outage as
// "alert disabled" — fail loud.
func IsOwnerAlertEnabled(tx sharedDomain.Transaction, configs notiRepo.AlertConfigRepository, gymID uuid.UUID, key alertDomain.Key) (bool, error) {
	cfg, err := configs.GetByGymAndKey(tx, gymID, key)
	if err != nil {
		return false, err
	}
	if cfg != nil {
		return cfg.Enabled, nil
	}
	def := alertDomain.LookupDefault(key)
	if def == nil {
		return false, nil
	}
	return def.EnabledByDefault, nil
}
