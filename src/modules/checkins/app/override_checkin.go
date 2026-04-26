package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/checkin"
	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// OverrideCheckinInput backs DA-29.2 ("dejar entrar"). The operator authorizes
// access for a member whose evaluation just denied them. We record a fresh
// checkin with method = the original method (so we can distinguish overrides
// over fingerprint vs PIN attempts) and result = allowed_override.
//
// We DO NOT modify the previous denied checkin row — the audit trail is
// "first row: denied, second row: override". Both stay queryable.
type OverrideCheckinInput struct {
	GymID          uuid.UUID
	OperatorID     uuid.UUID
	MemberID       uuid.UUID
	OriginalMethod string // "fingerprint" | "manual" | "pin"
	Reason         string
	Now            time.Time
}

// OverrideCheckin is the operator-driven override. We re-evaluate access
// status (in case the deny state changed during the conversation), but accept
// the override regardless — the operator is the human-in-the-loop who decides.
type OverrideCheckin struct {
	Members *memApp.MemberService
	Repo    chkRepo.CheckinRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
}

func NewOverrideCheckin(members *memApp.MemberService, repo chkRepo.CheckinRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *OverrideCheckin {
	return &OverrideCheckin{Members: members, Repo: repo, UoW: uow, Audit: recorder}
}

func (uc *OverrideCheckin) Execute(ctx context.Context, in OverrideCheckinInput) (*CheckinView, error) {
	if in.OperatorID == uuid.Nil {
		return nil, sharedDomain.NewValidationError(chkErrors.ErrOperatorRequired)
	}
	if len(in.Reason) < 5 {
		return nil, sharedDomain.NewValidationError(chkErrors.ErrOverrideReasonTooShort)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	method := in.OriginalMethod
	if method == "" {
		method = checkin.MethodManual
	}
	today := truncateToDay(now)

	var view *CheckinView
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// Re-fetch member info just for the response — we don't use the
		// status to decide anything (the operator decided).
		statusOut, err := uc.Members.GetAccessStatus(ctx, tx, memApp.GetAccessStatusInput{
			GymID: in.GymID, MemberID: in.MemberID, Today: today,
		})
		if err != nil {
			return err
		}

		c, err := checkin.NewOverrideCheckin(uuid.New(), in.GymID, in.MemberID, in.OperatorID, method, in.Reason, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Repo.Create(tx, c); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		op := in.OperatorID
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "checkins",
			EntityID:    c.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &op,
			Changes: map[string]any{
				"member_id":       in.MemberID.String(),
				"method":          method,
				"result":          c.Result,
				"manual_override": true,
				"override_reason": in.Reason,
				"original_status": string(statusOut.Status),
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})

		v := &CheckinView{
			CheckinID:    c.ID,
			MemberID:     in.MemberID,
			MemberName:   statusOut.Member.FullName,
			Method:       c.Method,
			Result:       c.Result,
			AccessStatus: statusOut.Status,
			Timestamp:    c.CheckinAt,
			Override:     true,
		}
		if statusOut.CurrentMembership != nil {
			exp := statusOut.CurrentMembership.ExpiryDate
			v.ExpiryDate = &exp
			d := statusOut.CurrentMembership.DaysUntilExpiry(today)
			v.DaysToExpiry = &d
		}
		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}
