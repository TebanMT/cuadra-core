package controllers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	categoryDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/category"
	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var errNotConfigured = errors.New("módulo de retos no está configurado en este servidor")
var errBadUUIDParam = errors.New("identificador inválido en la URL")

// bind is the local mirror of the helper used in other modules. Duplicating
// the four lines keeps the controller package self-contained instead of
// reaching into users/ for a generic helper.
func bind[T any](c *gin.Context, dst *T) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return false
	}
	if err := utils.ValidateRequest(*dst); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return false
	}
	return true
}

func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	raw := c.Param(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadUUIDParam)
		return uuid.Nil, false
	}
	return id, true
}

func parseInt(s string, dflt int) int {
	if s == "" {
		return dflt
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return dflt
	}
	return n
}

// toResp maps a domain Challenge to the wire DTO.
func toResp(c *challengeDomain.Challenge) challengeResp {
	return challengeResp{
		ID:                    c.ID,
		GymID:                 c.GymID,
		Version:               c.Version,
		Name:                  c.Name,
		Description:           c.Description,
		StartsAt:              c.StartsAt,
		MeasurementT0Deadline: c.MeasurementT0Deadline,
		MeasurementT1Start:    c.MeasurementT1Start,
		EndsAt:                c.EndsAt,
		Status:                c.Status,
		InscriptionFeeCents:   c.InscriptionFeeCents,
		InscriptionRefundable: c.InscriptionRefundable,
		MinWeeklyAttendance:   c.MinWeeklyAttendance,
		AttendanceGraceWeeks:  c.AttendanceGraceWeeks,
		StrengthCapPct:        c.StrengthCapPct,
		TieMarginIR:           c.TieMarginIR,
		BFFloorMalePct:        c.BFFloorMalePct,
		BFFloorFemalePct:      c.BFFloorFemalePct,
		CreatedAt:             c.CreatedAt,
		UpdatedAt:             c.UpdatedAt,
	}
}

func toCategoryResp(c *categoryDomain.Category) categoryResp {
	return categoryResp{
		ID:          c.ID,
		ChallengeID: c.ChallengeID,
		Name:        c.Name,
		SortOrder:   c.SortOrder,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

func toParticipantResp(p *participantDomain.Participant) participantResp {
	return participantResp{
		ID:                     p.ID,
		ChallengeID:            p.ChallengeID,
		MemberID:               p.MemberID,
		CategoryID:             p.CategoryID,
		ExerciseLegs:           p.ExerciseLegs,
		ExercisePush:           p.ExercisePush,
		ExercisePull:           p.ExercisePull,
		Status:                 p.Status,
		InscriptionFeePaid:     p.InscriptionFeePaid,
		InscriptionPaidAt:      p.InscriptionPaidAt,
		InscriptionRefundedAt:  p.InscriptionRefundedAt,
		DisqualificationReason: p.DisqualificationReason,
		DisqualifiedAt:         p.DisqualifiedAt,
		CreatedAt:              p.CreatedAt,
		UpdatedAt:              p.UpdatedAt,
	}
}

func toMeasurementResp(m *measurementDomain.Measurement) measurementResp {
	return measurementResp{
		ID:              m.ID,
		ParticipantID:   m.ParticipantID,
		Moment:          m.Moment,
		MeasuredAt:      m.MeasuredAt,
		BodyWeightKg:    m.BodyWeightKg,
		BodyFatPct:      m.BodyFatPct,
		LegsWeightKg:    m.LegsWeightKg,
		LegsReps:        m.LegsReps,
		PushWeightKg:    m.PushWeightKg,
		PushReps:        m.PushReps,
		PullWeightKg:    m.PullWeightKg,
		PullReps:        m.PullReps,
		Notes:           m.Notes,
		CreatedByUserID: m.CreatedByUserID,
		SupersededAt:    m.SupersededAt,
		SupersededByID:  m.SupersededByID,
		CreatedAt:       m.CreatedAt,
	}
}
