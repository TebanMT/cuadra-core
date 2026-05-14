package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	categoryDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/category"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ─── AddCategory ───────────────────────────────────────────────────────────

type AddCategory struct {
	Challenges challengeRepo.ChallengeRepository
	Categories challengeRepo.CategoryRepository
	UoW        sharedDomain.UnitOfWork
	Audit      audit.Recorder
	NowFunc    func() time.Time
}

func NewAddCategory(
	challenges challengeRepo.ChallengeRepository,
	categories challengeRepo.CategoryRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *AddCategory {
	return &AddCategory{
		Challenges: challenges, Categories: categories,
		UoW: uow, Audit: recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type AddCategoryInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ChallengeID uuid.UUID
	Name        string
	SortOrder   int
}

func (uc *AddCategory) Execute(ctx context.Context, in AddCategoryInput) (*categoryDomain.Category, error) {
	now := uc.NowFunc()
	var result *categoryDomain.Category
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		ch, err := uc.Challenges.GetByID(tx, in.ChallengeID)
		if err != nil {
			return err
		}
		if ch.GymID != in.GymID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		c, err := categoryDomain.NewCategory(uuid.New(), in.GymID, ch.ID, in.Name, in.SortOrder, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		saved, err := uc.Categories.Create(tx, c)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		result = saved
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenge_categories",
			EntityID:    saved.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"name": saved.Name, "challenge_id": ch.ID},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ─── UpdateCategory ────────────────────────────────────────────────────────

type UpdateCategory struct {
	Categories challengeRepo.CategoryRepository
	UoW        sharedDomain.UnitOfWork
	Audit      audit.Recorder
	NowFunc    func() time.Time
}

func NewUpdateCategory(
	categories challengeRepo.CategoryRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *UpdateCategory {
	return &UpdateCategory{
		Categories: categories, UoW: uow, Audit: recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type UpdateCategoryInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ChallengeID uuid.UUID
	CategoryID  uuid.UUID
	Name        *string
	SortOrder   *int
}

func (uc *UpdateCategory) Execute(ctx context.Context, in UpdateCategoryInput) (*categoryDomain.Category, error) {
	now := uc.NowFunc()
	var result *categoryDomain.Category
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		c, err := uc.Categories.GetByID(tx, in.CategoryID)
		if err != nil {
			return err
		}
		if c.GymID != in.GymID || c.ChallengeID != in.ChallengeID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		if in.Name != nil {
			if err := c.Rename(*in.Name, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
		}
		if in.SortOrder != nil {
			c.Reorder(*in.SortOrder, now)
		}
		saved, err := uc.Categories.Update(tx, c)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		result = saved
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenge_categories",
			EntityID:    saved.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"version": saved.Version},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ─── DeleteCategory ────────────────────────────────────────────────────────

type DeleteCategory struct {
	Categories challengeRepo.CategoryRepository
	UoW        sharedDomain.UnitOfWork
	Audit      audit.Recorder
	NowFunc    func() time.Time
}

func NewDeleteCategory(
	categories challengeRepo.CategoryRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *DeleteCategory {
	return &DeleteCategory{
		Categories: categories, UoW: uow, Audit: recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type DeleteCategoryInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ChallengeID uuid.UUID
	CategoryID  uuid.UUID
}

func (uc *DeleteCategory) Execute(ctx context.Context, in DeleteCategoryInput) error {
	now := uc.NowFunc()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		c, err := uc.Categories.GetByID(tx, in.CategoryID)
		if err != nil {
			return err
		}
		if c.GymID != in.GymID || c.ChallengeID != in.ChallengeID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		count, err := uc.Categories.CountParticipants(tx, c.ID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if count > 0 {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCategoryHasParticipants, "")
		}
		if err := uc.Categories.SoftDelete(tx, c.ID); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenge_categories",
			EntityID:    c.ID,
			Action:      audit.ActionDelete,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"name": c.Name},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
}

// ─── ListCategories ────────────────────────────────────────────────────────

type ListCategories struct {
	Challenges challengeRepo.ChallengeRepository
	Categories challengeRepo.CategoryRepository
	UoW        sharedDomain.UnitOfWork
}

func NewListCategories(
	challenges challengeRepo.ChallengeRepository,
	categories challengeRepo.CategoryRepository,
	uow sharedDomain.UnitOfWork,
) *ListCategories {
	return &ListCategories{Challenges: challenges, Categories: categories, UoW: uow}
}

type ListCategoriesInput struct {
	GymID       uuid.UUID
	ChallengeID uuid.UUID
}

func (uc *ListCategories) Execute(ctx context.Context, in ListCategoriesInput) ([]*categoryDomain.Category, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	ch, err := uc.Challenges.GetByID(tx, in.ChallengeID)
	if err != nil {
		return nil, err
	}
	if ch.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
	}
	out, err := uc.Categories.ListByChallenge(tx, ch.ID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return out, nil
}
