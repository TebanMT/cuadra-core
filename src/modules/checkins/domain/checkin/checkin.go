// Package checkin owns the Checkin aggregate (UC-029, UC-030, UC-032,
// ADR-002 §3.12).
//
// A Checkin records ONE access decision: who tried to enter, when, by which
// method (huella / número de socio / búsqueda manual), and what the access
// evaluator said at that moment. Past checkins are immutable — even if the
// member's plan status changes later, the historical row stays as it was.
package checkin

import (
	"time"

	"github.com/google/uuid"

	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
)

// Method enumerates the chk_checkins_method values (ADR-002 §3.12 + ADR-010:
// el método "pin" se renombró a "number" — el check-in por número de socio).
const (
	MethodFingerprint = "fingerprint"
	MethodManual      = "manual"
	MethodNumber      = "number"
)

// Result enumerates chk_checkins_result. The first 5 mirror access.AccessStatus
// 1:1; AllowedOverride is unique to checkins (the operator forced entry on a
// denied result — DA-29.2).
const (
	ResultAllowedActive          = "allowed_active"
	ResultAllowedExpiringSoon    = "allowed_expiring_soon"
	ResultAllowedOverride        = "allowed_override"
	ResultDeniedExpired          = "denied_expired"
	ResultDeniedInactive         = "denied_inactive"
	ResultDeniedNoMembership     = "denied_no_membership"
	ResultDeniedUnpaidEnrollment = "denied_unpaid_enrollment"
)

// Checkin is the aggregate. Construction is via the four New* factories below
// so callers can't accidentally combine fields that don't make sense (e.g.
// fingerprint + override — overrides are always operator-driven).
type Checkin struct {
	ID             uuid.UUID
	GymID          uuid.UUID
	Version        int
	MemberID       uuid.UUID
	CheckinAt      time.Time
	Method         string
	Result         string
	OperatorID     *uuid.UUID
	ManualOverride bool
	OverrideReason *string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// NewFingerprintCheckin (UC-029) — automatic, no operator. The operator is
// nil unless the result is an override (use NewOverrideCheckin for that).
func NewFingerprintCheckin(id, gymID, memberID uuid.UUID, status access.AccessStatus, now time.Time) (*Checkin, error) {
	res, err := resultFromStatus(status, false)
	if err != nil {
		return nil, err
	}
	return &Checkin{
		ID: id, GymID: gymID, Version: 1,
		MemberID:  memberID,
		CheckinAt: now, Method: MethodFingerprint, Result: res,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// NewNumberCheckin (UC-032, ADR-010) — automatic, no operator. Check-in por
// número de socio. Same shape as fingerprint.
func NewNumberCheckin(id, gymID, memberID uuid.UUID, status access.AccessStatus, now time.Time) (*Checkin, error) {
	res, err := resultFromStatus(status, false)
	if err != nil {
		return nil, err
	}
	return &Checkin{
		ID: id, GymID: gymID, Version: 1,
		MemberID:  memberID,
		CheckinAt: now, Method: MethodNumber, Result: res,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// NewManualCheckin (UC-030) — operator searched and confirmed. operatorID is
// recorded for audit (who let this member in?).
func NewManualCheckin(id, gymID, memberID, operatorID uuid.UUID, status access.AccessStatus, now time.Time) (*Checkin, error) {
	if operatorID == uuid.Nil {
		return nil, chkErrors.ErrOperatorRequired
	}
	res, err := resultFromStatus(status, false)
	if err != nil {
		return nil, err
	}
	op := operatorID
	return &Checkin{
		ID: id, GymID: gymID, Version: 1,
		MemberID:  memberID,
		CheckinAt: now, Method: MethodManual, Result: res,
		OperatorID: &op,
		CreatedAt:  now, UpdatedAt: now,
	}, nil
}

// NewOverrideCheckin (DA-29.2) — operator forced entry on a denied evaluation.
// The original method is preserved (the operator probably overrode after a
// failed fingerprint or PIN attempt). Reason is mandatory and length-checked
// — we want enough breadcrumb to investigate disputes later.
func NewOverrideCheckin(id, gymID, memberID, operatorID uuid.UUID, originalMethod string, reason string, now time.Time) (*Checkin, error) {
	if operatorID == uuid.Nil {
		return nil, chkErrors.ErrOperatorRequired
	}
	if !isKnownMethod(originalMethod) {
		return nil, chkErrors.ErrInvalidMethod
	}
	if len(reason) < 5 {
		return nil, chkErrors.ErrOverrideReasonTooShort
	}
	op := operatorID
	r := reason
	return &Checkin{
		ID: id, GymID: gymID, Version: 1,
		MemberID:  memberID,
		CheckinAt: now, Method: originalMethod, Result: ResultAllowedOverride,
		OperatorID:     &op,
		ManualOverride: true,
		OverrideReason: &r,
		CreatedAt:      now, UpdatedAt: now,
	}, nil
}

// IsAllowed is a convenience helper for UI / tests — true for any of the
// "allowed_*" results (including override).
func (c *Checkin) IsAllowed() bool {
	switch c.Result {
	case ResultAllowedActive, ResultAllowedExpiringSoon, ResultAllowedOverride:
		return true
	default:
		return false
	}
}

// resultFromStatus converts the members BC's AccessStatus into the chk_checkins_result
// string. The two enums overlap on the first 5 values; we keep them as
// separate types so future drift in either is caught at compile time.
func resultFromStatus(s access.AccessStatus, override bool) (string, error) {
	if override {
		return ResultAllowedOverride, nil
	}
	switch s {
	case access.AllowedActive:
		return ResultAllowedActive, nil
	case access.AllowedExpiringSoon:
		return ResultAllowedExpiringSoon, nil
	case access.DeniedExpired:
		return ResultDeniedExpired, nil
	case access.DeniedInactive:
		return ResultDeniedInactive, nil
	case access.DeniedNoMembership:
		return ResultDeniedNoMembership, nil
	case access.DeniedUnpaidEnrollment:
		return ResultDeniedUnpaidEnrollment, nil
	default:
		return "", chkErrors.ErrUnknownAccessStatus
	}
}

func isKnownMethod(m string) bool {
	return m == MethodFingerprint || m == MethodManual || m == MethodNumber
}
