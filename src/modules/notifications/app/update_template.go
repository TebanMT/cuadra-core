package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// UpdateTemplateInput backs UC-038 DA-38.2 (PATCH
// /api/v1/notification-templates/:key). The owner edits the visible copy;
// the variable set is enforced by the domain validator.
type UpdateTemplateInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Key         string
	Body        string
	Enabled     *bool
}

type UpdateTemplate struct {
	Templates notiRepo.TemplateOverrideRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
}

func NewUpdateTemplate(templates notiRepo.TemplateOverrideRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateTemplate {
	return &UpdateTemplate{Templates: templates, UoW: uow, Audit: recorder}
}

func (uc *UpdateTemplate) Execute(ctx context.Context, in UpdateTemplateInput) (*tplDomain.Override, error) {
	if tplDomain.LookupDefault(in.Key) == nil {
		return nil, sharedDomain.NewValidationError(notiErrors.ErrTemplateNotFound)
	}
	now := time.Now().UTC()
	var out *tplDomain.Override
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		existing, err := uc.Templates.GetByGymAndKey(tx, in.GymID, in.Key)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var override *tplDomain.Override
		if existing == nil {
			override, err = tplDomain.NewOverride(uuid.New(), in.GymID, in.Key, in.Body, now)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if in.Enabled != nil {
				override.Enabled = *in.Enabled
			}
		} else {
			override = existing
			if err := override.Edit(in.Body, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if in.Enabled != nil {
				override.SetEnabled(*in.Enabled, now)
			}
		}
		saved, err := uc.Templates.Upsert(tx, override)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "notification_templates",
			EntityID:    saved.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"template_key": saved.TemplateKey,
				"body":         saved.Body,
				"enabled":      saved.Enabled,
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

// ListTemplates returns the merged view: each system template + the gym's
// override (if any). Sorted by key.
type ListTemplates struct {
	Templates notiRepo.TemplateOverrideRepository
	UoW       sharedDomain.UnitOfWork
}

func NewListTemplates(templates notiRepo.TemplateOverrideRepository, uow sharedDomain.UnitOfWork) *ListTemplates {
	return &ListTemplates{Templates: templates, UoW: uow}
}

// TemplateView is one row of the listing.
type TemplateView struct {
	Key       string
	Channel   string
	Category  string
	Variables []string
	Default   string
	Body      string
	Enabled   bool
	Custom    bool
}

func (uc *ListTemplates) Execute(ctx context.Context, gymID uuid.UUID) ([]TemplateView, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	overrides, err := uc.Templates.ListByGym(tx, gymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	byKey := make(map[string]*tplDomain.Override, len(overrides))
	for _, o := range overrides {
		byKey[o.TemplateKey] = o
	}
	defaults := tplDomain.DefaultLibrary()
	out := make([]TemplateView, 0, len(defaults))
	for _, d := range defaults {
		view := TemplateView{
			Key:       d.Key,
			Channel:   string(d.Channel),
			Category:  string(d.Category),
			Variables: append([]string(nil), d.Variables...),
			Default:   d.Body,
			Body:      d.Body,
			Enabled:   true,
		}
		if o, ok := byKey[d.Key]; ok {
			view.Body = o.Body
			view.Enabled = o.Enabled
			view.Custom = true
		}
		out = append(out, view)
	}
	return out, nil
}
