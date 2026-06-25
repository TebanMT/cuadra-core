// Package app holds the checkins use cases (UC-029, UC-030, UC-031, UC-032)
// + the cross-BC seam other modules invoke (none in MVP — checkins is a leaf).
//
// Conventions copied from Sesión 1-4:
//   - one struct per use case, constructor New<UC>().
//   - Execute(ctx, input) → (output, error).
//   - all writes inside UoW.Command for auto-rollback + audit consistency.
//   - errors are CustomError (validation/business/unexpected); the controller
//     maps them to HTTP via shared/utils.DomainErrorToHttpCode.
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/checkin"
	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CheckinView is what every checkin use case returns to the controller. Carries
// enough info to render the kiosko feedback (member name, expiry days, result
// state) without an extra round-trip.
type CheckinView struct {
	CheckinID    uuid.UUID
	MemberID     uuid.UUID
	MemberName   string
	Method       string
	Result       string
	AccessStatus access.AccessStatus
	ExpiryDate   *time.Time
	DaysToExpiry *int
	Timestamp    time.Time
	Override     bool
}

// recordCheckin is the shared helper UC-029 / UC-030 / UC-032 funnel through
// after they've identified a member by their preferred method. It evaluates
// access status + persists + audits, all inside the same Command tx.
func recordCheckin(
	ctx context.Context,
	tx sharedDomain.Transaction,
	memberSvc *memApp.MemberService,
	gyms gymRepo.GymRepository,
	repo chkRepo.CheckinRepository,
	recorder audit.Recorder,
	gymID, memberID uuid.UUID,
	method string,
	operatorID *uuid.UUID,
	now time.Time,
) (*CheckinView, error) {
	// `today` debe ser el día CALENDARIO del gym en SU zona horaria, no UTC:
	// las membresías vencen a medianoche (date-only) y comparar contra el día
	// UTC rechazaba a un socio en su ÚLTIMO día de vigencia en la tarde-noche
	// (ya cruzó medianoche UTC). gymLocalToday resuelve la tz del gym.
	today := gymLocalToday(tx, gyms, gymID, now)
	statusOut, err := memberSvc.GetAccessStatus(ctx, tx, memApp.GetAccessStatusInput{
		GymID:    gymID,
		MemberID: memberID,
		Today:    today,
	})
	if err != nil {
		return nil, err
	}

	var c *checkin.Checkin
	switch method {
	case checkin.MethodFingerprint:
		c, err = checkin.NewFingerprintCheckin(uuid.New(), gymID, memberID, statusOut.Status, now)
	case checkin.MethodNumber:
		c, err = checkin.NewNumberCheckin(uuid.New(), gymID, memberID, statusOut.Status, now)
	case checkin.MethodManual:
		if operatorID == nil {
			return nil, sharedDomain.NewValidationError(chkErrors.ErrOperatorRequired)
		}
		c, err = checkin.NewManualCheckin(uuid.New(), gymID, memberID, *operatorID, statusOut.Status, now)
	default:
		return nil, sharedDomain.NewValidationError(chkErrors.ErrInvalidMethod)
	}
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	if _, err := repo.Create(tx, c); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	_ = recorder.Record(ctx, tx, audit.Entry{
		GymID:       gymID,
		EntityType:  "checkins",
		EntityID:    c.ID,
		Action:      audit.ActionCreate,
		ActorUserID: operatorID,
		Changes: map[string]any{
			"member_id":       memberID.String(),
			"method":          method,
			"result":          c.Result,
			"manual_override": false,
		},
		IPAddress: audit.IPFromContext(ctx),
		UserAgent: audit.UAFromContext(ctx),
		At:        now,
	})

	view := &CheckinView{
		CheckinID:    c.ID,
		MemberID:     memberID,
		MemberName:   statusOut.Member.FullName,
		Method:       c.Method,
		Result:       c.Result,
		AccessStatus: statusOut.Status,
		Timestamp:    c.CheckinAt,
	}
	if statusOut.CurrentMembership != nil {
		// ExpiryDate puede ser nil cuando la membership está en
		// pending_payment — el view ya tiene el tipo *time.Time, sólo
		// propagamos el puntero.
		view.ExpiryDate = statusOut.CurrentMembership.ExpiryDate
		if statusOut.CurrentMembership.ExpiryDate != nil {
			days := statusOut.CurrentMembership.DaysUntilExpiry(today)
			view.DaysToExpiry = &days
		}
	}
	return view, nil
}

// truncateToDay drops the time-of-day so AccessStatusEvaluator does the right
// thing regardless of when in the day the checkin happens. UTC fallback.
func truncateToDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// gymLocalToday devuelve el día calendario del gym en SU zona horaria,
// expresado como medianoche UTC (la convención date-only que usan las
// fechas de vigencia). Carga el gym para leer su Timezone; si no hay repo
// cableado, el gym no existe, o la tz es inválida, cae al día UTC
// (comportamiento previo) — nunca rompe el check-in por esto.
func gymLocalToday(tx sharedDomain.Transaction, gyms gymRepo.GymRepository, gymID uuid.UUID, now time.Time) time.Time {
	if gyms == nil {
		return truncateToDay(now)
	}
	g, err := gyms.GetByID(tx, gymID)
	if err != nil || g == nil || g.Timezone == "" {
		return truncateToDay(now)
	}
	loc, err := time.LoadLocation(g.Timezone)
	if err != nil {
		return truncateToDay(now)
	}
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}
