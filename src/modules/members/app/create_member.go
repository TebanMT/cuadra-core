package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CreateMemberInput backs UC-012. The only required fields are GymID +
// FullName + Phone + MembershipTypeID. Optional fields use pointers (nil ==
// "not provided"). When `AllowDuplicatePhone` is false and the phone already
// exists the use case returns a BusinessError; the front-end can re-call with
// AllowDuplicatePhone=true to confirm (DA-12.3).
type CreateMemberInput struct {
	GymID               uuid.UUID
	ActorUserID         uuid.UUID
	FullName            string
	Phone               string
	Email               *string
	Birthdate           *time.Time
	PhotoURL            *string
	Notes               *string
	MembershipTypeID    uuid.UUID
	StartDate           time.Time
	AllowDuplicatePhone bool
	// ChargeFirstPayment marks the "Cobrar primer pago ahora" toggle (DA-12.1).
	// When true the use case emits the MemberCreatedWithInitialPayment event
	// (returned in Output.PendingFirstPayment); billing's UC-018 will pick it
	// up in Sesión 3.
	ChargeFirstPayment bool
}

// CreateMemberOutput contains the new member's id and folio plus a flag that
// signals billing it should immediately register the first payment.
type CreateMemberOutput struct {
	MemberID            uuid.UUID
	MembershipID        uuid.UUID
	Folio               string
	ExpiryDate          time.Time
	PendingFirstPayment bool
}

type CreateMember struct {
	Members         memRepo.MemberRepository
	Memberships     memRepo.MembershipRepository
	MembershipTypes memRepo.MembershipTypeRepository
	UoW             sharedDomain.UnitOfWork
	Audit           audit.Recorder
}

func NewCreateMember(members memRepo.MemberRepository, memberships memRepo.MembershipRepository,
	mtypes memRepo.MembershipTypeRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateMember {
	return &CreateMember{
		Members:         members,
		Memberships:     memberships,
		MembershipTypes: mtypes,
		UoW:             uow,
		Audit:           recorder,
	}
}

func (uc *CreateMember) Execute(ctx context.Context, in CreateMemberInput) (*CreateMemberOutput, error) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if err := memberDomain.ValidateStartDate(in.StartDate, today); err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}

	var out CreateMemberOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// 1) Type must exist + belong to gym + be active.
		mt, err := uc.MembershipTypes.GetByID(tx, in.MembershipTypeID)
		if err != nil {
			return err
		}
		if mt.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeNotFound, "")
		}
		if !mt.Active {
			return sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeInactive, "")
		}

		// 2) Phone uniqueness — soft warning unless caller acknowledged.
		phoneNorm, err := normalizeAndValidatePhone(in.Phone)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if !in.AllowDuplicatePhone {
			exists, err := uc.Members.ExistsByGymAndPhone(tx, in.GymID, phoneNorm)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if exists {
				return sharedDomain.NewBusinessError(memErrors.ErrPhoneAlreadyExists, "")
			}
		}

		// 3) Folio.
		folio, err := uc.Members.NextFolio(tx, in.GymID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 4) Build domain Member.
		mID := uuid.New()
		m, err := memberDomain.NewMember(mID, in.GymID, folio, in.FullName, phoneNorm, in.ActorUserID, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		// Apply optional fields via ProfileUpdate (re-uses validation).
		if err := m.ApplyProfileUpdate(memberDomain.ProfileUpdate{
			Email:     in.Email,
			Birthdate: in.Birthdate,
			PhotoURL:  in.PhotoURL,
			Notes:     in.Notes,
		}, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}

		if _, err := uc.Members.Create(tx, m); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 5) Active Membership.
		ms := membershipDomain.New(uuid.New(), in.GymID, m.ID, mt, in.StartDate, now)
		if _, err := uc.Memberships.Create(tx, ms); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 6) Audit.
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "members",
			EntityID:    m.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"folio":              m.Folio,
				"membership_type_id": mt.ID,
				"membership_id":      ms.ID,
				"expiry_date":        ms.ExpiryDate.Format("2006-01-02"),
				"first_payment":      in.ChargeFirstPayment,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = CreateMemberOutput{
			MemberID:            m.ID,
			MembershipID:        ms.ID,
			Folio:               m.Folio,
			ExpiryDate:          ms.ExpiryDate,
			PendingFirstPayment: in.ChargeFirstPayment,
		}
		// TODO(billing — Sesión 3): si in.ChargeFirstPayment == true, emitir
		// evento `members.MemberCreatedWithInitialPayment` para que UC-018 lo
		// procese aquí mismo (mismo UoW.Command). En Sesión 2 no existe
		// `billing` aún; el handler señaliza el flag al frontend.
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func normalizeAndValidatePhone(raw string) (string, error) {
	v := raw
	v = trimSpaces(v)
	if !memberDomain.ValidatePhone(v) {
		return "", memErrors.ErrInvalidPhone
	}
	return v, nil
}

func trimSpaces(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '-', '(', ')':
			continue
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
