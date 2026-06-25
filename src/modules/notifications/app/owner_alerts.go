package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	alertDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/alertconfig"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// AlertConfigView is one row of the GET /owner-alerts payload. Each alert
// carries both the toggle (Enabled) and its mirror WhatsApp template
// (TemplateKey + Variables + DefaultBody + Body + IsDefault) so the FE
// renders toggle and editable copy from a single read. Version mirrors the
// underlying alertconfig.Config.Version — exposed for tests and idempotency.
type AlertConfigView struct {
	Key         alertDomain.Key
	Enabled     bool
	Description string
	Version     int

	TemplateKey string
	Variables   []string
	DefaultBody string
	Body        string
	// IsDefault is true when the gym has no per-gym template override —
	// FE renders the "Personalizada" badge off `!IsDefault`.
	IsDefault bool
}

// ListOwnerAlerts merges the canonical alert library with the gym's
// per-key overrides AND the matching template (default + per-gym override).
// UC-040 DA-40.1.
type ListOwnerAlerts struct {
	Configs   notiRepo.AlertConfigRepository
	Templates notiRepo.TemplateOverrideRepository
	UoW       sharedDomain.UnitOfWork
}

func NewListOwnerAlerts(
	configs notiRepo.AlertConfigRepository,
	templates notiRepo.TemplateOverrideRepository,
	uow sharedDomain.UnitOfWork,
) *ListOwnerAlerts {
	return &ListOwnerAlerts{Configs: configs, Templates: templates, UoW: uow}
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
	cfgByKey := make(map[alertDomain.Key]*alertDomain.Config, len(overrides))
	for _, o := range overrides {
		cfgByKey[o.Key] = o
	}
	tplOverrides, err := uc.Templates.ListByGym(tx, gymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	tplByKey := make(map[string]*tplDomain.Override, len(tplOverrides))
	for _, o := range tplOverrides {
		tplByKey[o.TemplateKey] = o
	}

	defaults := alertDomain.Defaults()
	out := make([]AlertConfigView, 0, len(defaults))
	for _, d := range defaults {
		view, err := buildAlertView(d, cfgByKey[d.Key], tplByKey)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return out, nil
}

// UpdateOwnerAlertInput backs PATCH /api/v1/owner-alerts/:key. Both
// Enabled and Body are optional — at least one must be provided. The key
// must be one of alertconfig.Defaults; unknown keys are rejected so a
// stale FE build can't quietly create dead rows.
type UpdateOwnerAlertInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Key         alertDomain.Key
	Enabled     *bool
	Body        *string
}

type UpdateOwnerAlert struct {
	Configs   notiRepo.AlertConfigRepository
	Templates notiRepo.TemplateOverrideRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	// CanEditBody: ver UpdateTemplate. Sin Plus, el texto de la alerta se
	// fuerza al default aprobado por Meta; el switch on/off sí se respeta.
	CanEditBody func(ctx context.Context, gymID uuid.UUID) bool
}

func NewUpdateOwnerAlert(
	configs notiRepo.AlertConfigRepository,
	templates notiRepo.TemplateOverrideRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *UpdateOwnerAlert {
	return &UpdateOwnerAlert{Configs: configs, Templates: templates, UoW: uow, Audit: recorder}
}

func (uc *UpdateOwnerAlert) Execute(ctx context.Context, in UpdateOwnerAlertInput) (*AlertConfigView, error) {
	alertDef := alertDomain.LookupDefault(in.Key)
	if alertDef == nil {
		return nil, sharedDomain.NewValidationError(notiErrors.ErrAlertKeyUnknown)
	}
	if in.Enabled == nil && in.Body == nil {
		return nil, sharedDomain.NewValidationError(notiErrors.ErrAlertUpdateEmpty)
	}
	tplKey, ok := OwnerAlertTemplateKey(in.Key)
	if !ok {
		// Defensive: alertconfig.Defaults and OwnerAlertTemplateKey are kept
		// in lockstep. If they ever drift, surface it as a clear error.
		return nil, sharedDomain.NewUnexpectedError(notiErrors.ErrTemplateNotFound)
	}
	tplDef := tplDomain.LookupDefault(tplKey)
	if tplDef == nil {
		return nil, sharedDomain.NewUnexpectedError(notiErrors.ErrTemplateNotFound)
	}

	now := time.Now().UTC()
	var (
		savedCfg *alertDomain.Config
		savedTpl *tplDomain.Override
	)
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// Toggle path — only when Enabled was provided.
		if in.Enabled != nil {
			existing, err := uc.Configs.GetByGymAndKey(tx, in.GymID, in.Key)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			var cfg *alertDomain.Config
			if existing == nil {
				cfg, err = alertDomain.New(in.GymID, in.Key, *in.Enabled, now)
				if err != nil {
					return sharedDomain.NewValidationError(err)
				}
			} else {
				cfg = existing
				cfg.SetEnabled(*in.Enabled, now)
			}
			cfg, err = uc.Configs.Upsert(tx, cfg)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			savedCfg = cfg
			// Audit entry per surface — owner_alert_configs.
			_ = uc.Audit.Record(ctx, tx, audit.Entry{
				GymID:       in.GymID,
				EntityType:  "owner_alert_configs",
				EntityID:    in.GymID,
				Action:      audit.ActionUpdate,
				ActorUserID: &in.ActorUserID,
				Changes: map[string]any{
					"alert_key": string(cfg.Key),
					"enabled":   cfg.Enabled,
					"version":   cfg.Version,
				},
				IPAddress: audit.IPFromContext(ctx),
				UserAgent: audit.UAFromContext(ctx),
				At:        now,
			})
		} else {
			// Read current config for the response view.
			existing, err := uc.Configs.GetByGymAndKey(tx, in.GymID, in.Key)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			savedCfg = existing
		}

		// Template path — only when Body was provided. Validates against
		// the canonical Definition (variables FIXED, copy editable).
		if in.Body != nil {
			// Sin Plus el texto no se personaliza: lo forzamos al default
			// aprobado por Meta (el switch on/off sí se respetó arriba).
			body := *in.Body
			if uc.CanEditBody != nil && !uc.CanEditBody(ctx, in.GymID) {
				body = tplDef.Body
			}
			existing, err := uc.Templates.GetByGymAndKey(tx, in.GymID, tplKey)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			var override *tplDomain.Override
			if existing == nil {
				override, err = tplDomain.NewOverride(uuid.New(), in.GymID, tplKey, body, now)
				if err != nil {
					return sharedDomain.NewValidationError(err)
				}
			} else {
				override = existing
				if err := override.Edit(body, now); err != nil {
					return sharedDomain.NewValidationError(err)
				}
			}
			override, err = uc.Templates.Upsert(tx, override)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			savedTpl = override
			_ = uc.Audit.Record(ctx, tx, audit.Entry{
				GymID:       in.GymID,
				EntityType:  "notification_templates",
				EntityID:    override.ID,
				Action:      audit.ActionUpdate,
				ActorUserID: &in.ActorUserID,
				Changes: map[string]any{
					"template_key": override.TemplateKey,
					"body":         override.Body,
					"enabled":      override.Enabled,
				},
				IPAddress: audit.IPFromContext(ctx),
				UserAgent: audit.UAFromContext(ctx),
				At:        now,
			})
		} else {
			// Read current override for the response view.
			existing, err := uc.Templates.GetByGymAndKey(tx, in.GymID, tplKey)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			savedTpl = existing
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	view := assembleAlertView(*alertDef, savedCfg, *tplDef, savedTpl)
	return &view, nil
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

// buildAlertView resolves the matching template (default + override map) and
// returns the merged view. Used by ListOwnerAlerts where we already fetched
// all overrides up-front.
func buildAlertView(
	def alertDomain.Definition,
	cfg *alertDomain.Config,
	tplByKey map[string]*tplDomain.Override,
) (AlertConfigView, error) {
	tplKey, ok := OwnerAlertTemplateKey(def.Key)
	if !ok {
		return AlertConfigView{}, sharedDomain.NewUnexpectedError(notiErrors.ErrTemplateNotFound)
	}
	tplDef := tplDomain.LookupDefault(tplKey)
	if tplDef == nil {
		return AlertConfigView{}, sharedDomain.NewUnexpectedError(notiErrors.ErrTemplateNotFound)
	}
	return assembleAlertView(def, cfg, *tplDef, tplByKey[tplKey]), nil
}

// assembleAlertView is the single place that merges defaults with overrides
// into the wire shape. Keeping it tiny ensures GET and PATCH echo identical
// payloads (the FE relies on that to render without a re-fetch).
func assembleAlertView(
	alertDef alertDomain.Definition,
	cfg *alertDomain.Config,
	tplDef tplDomain.Definition,
	tplOverride *tplDomain.Override,
) AlertConfigView {
	view := AlertConfigView{
		Key:         alertDef.Key,
		Enabled:     alertDef.EnabledByDefault,
		Description: alertDef.Description,
		Version:     0,

		TemplateKey: tplDef.Key,
		Variables:   append([]string(nil), tplDef.Variables...),
		DefaultBody: tplDef.Body,
		Body:        tplDef.Body,
		IsDefault:   true,
	}
	if cfg != nil {
		view.Enabled = cfg.Enabled
		view.Version = cfg.Version
	}
	if tplOverride != nil {
		view.Body = tplOverride.Body
		view.IsDefault = false
	}
	return view
}
