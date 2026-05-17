// Package controllers exposes HTTP handlers for the challenges (retos)
// bounded context. The same controller is registered on both the cloud
// server and the sidecar (DI in cmd/server/main.go and cmd/sidecar/main.go
// respectively). Use cases run against whichever repo flavor the wiring
// layer injected.
package controllers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	challengesApp "github.com/cuadra/cuadra-core/src/modules/challenges/app"
	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// ChallengeController bundles every retos endpoint. Use cases are
// optional (nil-safe) so the wiring layer can ship endpoints in phases.
type ChallengeController struct {
	CreateChallenge         *challengesApp.CreateChallenge
	ListChallenges          *challengesApp.ListChallenges
	GetChallengeDetail      *challengesApp.GetChallengeDetail
	UpdateChallengeConfig   *challengesApp.UpdateChallengeConfig
	TransitionStatus        *challengesApp.TransitionChallengeStatus
	AddCategory             *challengesApp.AddCategory
	UpdateCategory          *challengesApp.UpdateCategory
	DeleteCategory          *challengesApp.DeleteCategory
	ListCategories          *challengesApp.ListCategories
	AddParticipant          *challengesApp.AddParticipant
	UpdateParticipant       *challengesApp.UpdateParticipant
	RemoveParticipant       *challengesApp.RemoveParticipant
	ListParticipants        *challengesApp.ListParticipants
	CaptureMeasurement      *challengesApp.CaptureMeasurement
	ListMeasurements        *challengesApp.ListMeasurements
	GetChallengeRanking     *challengesApp.GetChallengeRanking
	GetAttendanceReport     *challengesApp.GetAttendanceReport
	CheckDisqualifications *challengesApp.CheckDisqualifications
	Tokens                 auth.TokenService
	// PlanGate (opcional) corre justo después del AuthMiddleware y aborta
	// con 402 si el gym no tiene Plus / trial. Retos es feature Plus por
	// pricing: gym de barrio puede operar sin gamification. Cuando es nil
	// (tests, dev local), no se aplica.
	PlanGate gin.HandlerFunc
}

func NewChallengeController(deps ChallengeController) *ChallengeController {
	c := deps
	return &c
}

// RegisterRoutes wires every endpoint under /api/v1/challenges. Owner-only
// mutations get RequireOwner in addition to AuthMiddleware; reads + same-day
// operator workflows (capturing mediciones) are open to all authenticated
// roles.
func (ctrl *ChallengeController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1/challenges")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	if ctrl.PlanGate != nil {
		api.Use(ctrl.PlanGate)
	}
	{
		api.GET("", ctrl.handleList)
		api.GET("/:id", ctrl.handleDetail)
		api.GET("/:id/categories", ctrl.handleListCategories)
		api.GET("/:id/participants", ctrl.handleListParticipants)
		api.GET("/:id/participants/:pid/measurements", ctrl.handleListMeasurements)
		api.POST("/:id/participants/:pid/measurements", ctrl.handleCaptureMeasurement)
		api.GET("/:id/ranking", ctrl.handleRanking)
		api.GET("/:id/attendance-status", ctrl.handleAttendance)
	}

	owners := api.Group("")
	owners.Use(middleware.RequireOwner())
	{
		owners.POST("", ctrl.handleCreate)
		owners.PATCH("/:id", ctrl.handleUpdateConfig)
		owners.POST("/:id/status", ctrl.handleTransition)
		owners.POST("/:id/categories", ctrl.handleAddCategory)
		owners.PATCH("/:id/categories/:catId", ctrl.handleUpdateCategory)
		owners.DELETE("/:id/categories/:catId", ctrl.handleDeleteCategory)
		owners.POST("/:id/participants", ctrl.handleAddParticipant)
		owners.PATCH("/:id/participants/:pid", ctrl.handleUpdateParticipant)
		owners.DELETE("/:id/participants/:pid", ctrl.handleRemoveParticipant)
		owners.POST("/:id/check-disqualifications", ctrl.handleCheckDisqualifications)
	}
}

// ─── DTOs ──────────────────────────────────────────────────────────────────

type createChallengeReq struct {
	Name                  string    `json:"name" validate:"required,min=3,max=120"`
	Description           string    `json:"description"`
	StartsAt              time.Time `json:"starts_at" validate:"required"`
	MeasurementT0Deadline time.Time `json:"measurement_t0_deadline" validate:"required"`
	MeasurementT1Start    time.Time `json:"measurement_t1_start" validate:"required"`
	EndsAt                time.Time `json:"ends_at" validate:"required"`
	InscriptionFeeCents   *int      `json:"inscription_fee_cents,omitempty"`
	InscriptionRefundable *bool     `json:"inscription_refundable,omitempty"`
	MinWeeklyAttendance   *int      `json:"min_weekly_attendance,omitempty"`
	AttendanceGraceWeeks  *int      `json:"attendance_grace_weeks,omitempty"`
	StrengthCapPct        *float64  `json:"strength_cap_pct,omitempty"`
	TieMarginIR           *float64  `json:"tie_margin_ir,omitempty"`
	BFFloorMalePct        *float64  `json:"bf_floor_male_pct,omitempty"`
	BFFloorFemalePct      *float64  `json:"bf_floor_female_pct,omitempty"`
}

type updateChallengeReq struct {
	Name                  *string    `json:"name,omitempty"`
	Description           *string    `json:"description,omitempty"`
	StartsAt              *time.Time `json:"starts_at,omitempty"`
	MeasurementT0Deadline *time.Time `json:"measurement_t0_deadline,omitempty"`
	MeasurementT1Start    *time.Time `json:"measurement_t1_start,omitempty"`
	EndsAt                *time.Time `json:"ends_at,omitempty"`
	InscriptionFeeCents   *int       `json:"inscription_fee_cents,omitempty"`
	InscriptionRefundable *bool      `json:"inscription_refundable,omitempty"`
	MinWeeklyAttendance   *int       `json:"min_weekly_attendance,omitempty"`
	AttendanceGraceWeeks  *int       `json:"attendance_grace_weeks,omitempty"`
	StrengthCapPct        *float64   `json:"strength_cap_pct,omitempty"`
	TieMarginIR           *float64   `json:"tie_margin_ir,omitempty"`
	BFFloorMalePct        *float64   `json:"bf_floor_male_pct,omitempty"`
	BFFloorFemalePct      *float64   `json:"bf_floor_female_pct,omitempty"`
}

type transitionReq struct {
	Transition string `json:"transition" validate:"required"`
}

type categoryReq struct {
	Name      string `json:"name" validate:"required,min=1,max=80"`
	SortOrder int    `json:"sort_order"`
}

type updateCategoryReq struct {
	Name      *string `json:"name,omitempty"`
	SortOrder *int    `json:"sort_order,omitempty"`
}

type addParticipantReq struct {
	MemberID     uuid.UUID `json:"member_id" validate:"required"`
	CategoryID   uuid.UUID `json:"category_id" validate:"required"`
	ExerciseLegs string    `json:"exercise_legs"`
	ExercisePush string    `json:"exercise_push"`
	ExercisePull string    `json:"exercise_pull"`
}

type updateParticipantReq struct {
	ExerciseLegs *string `json:"exercise_legs,omitempty"`
	ExercisePush *string `json:"exercise_push,omitempty"`
	ExercisePull *string `json:"exercise_pull,omitempty"`
	MarkFeePaid  bool    `json:"mark_fee_paid"`
	Withdraw     bool    `json:"withdraw"`
}

type captureMeasurementReq struct {
	Moment       string    `json:"moment" validate:"required"`
	MeasuredAt   time.Time `json:"measured_at" validate:"required"`
	BodyWeightKg float64   `json:"body_weight_kg" validate:"required"`
	BodyFatPct   float64   `json:"body_fat_pct" validate:"required"`
	LegsWeightKg float64   `json:"legs_weight_kg" validate:"required"`
	LegsReps     int       `json:"legs_reps" validate:"required"`
	PushWeightKg float64   `json:"push_weight_kg" validate:"required"`
	PushReps     int       `json:"push_reps" validate:"required"`
	PullWeightKg float64   `json:"pull_weight_kg" validate:"required"`
	PullReps     int       `json:"pull_reps" validate:"required"`
	Notes        string    `json:"notes"`
}

// challengeResp mirrors the wire shape both the desktop and the dashboard
// consume. Keeping the field names snake_case and explicit makes the TS
// side a plain interface with no transformation layer.
type challengeResp struct {
	ID                    uuid.UUID `json:"id"`
	GymID                 uuid.UUID `json:"gym_id"`
	Version               int       `json:"version"`
	Name                  string    `json:"name"`
	Description           string    `json:"description"`
	StartsAt              time.Time `json:"starts_at"`
	MeasurementT0Deadline time.Time `json:"measurement_t0_deadline"`
	MeasurementT1Start    time.Time `json:"measurement_t1_start"`
	EndsAt                time.Time `json:"ends_at"`
	Status                string    `json:"status"`
	InscriptionFeeCents   int       `json:"inscription_fee_cents"`
	InscriptionRefundable bool      `json:"inscription_refundable"`
	MinWeeklyAttendance   int       `json:"min_weekly_attendance"`
	AttendanceGraceWeeks  int       `json:"attendance_grace_weeks"`
	StrengthCapPct        float64   `json:"strength_cap_pct"`
	TieMarginIR           float64   `json:"tie_margin_ir"`
	BFFloorMalePct        float64   `json:"bf_floor_male_pct"`
	BFFloorFemalePct      float64   `json:"bf_floor_female_pct"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type categoryResp struct {
	ID          uuid.UUID `json:"id"`
	ChallengeID uuid.UUID `json:"challenge_id"`
	Name        string    `json:"name"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type participantResp struct {
	ID                     uuid.UUID  `json:"id"`
	ChallengeID            uuid.UUID  `json:"challenge_id"`
	MemberID               uuid.UUID  `json:"member_id"`
	CategoryID             uuid.UUID  `json:"category_id"`
	ExerciseLegs           string     `json:"exercise_legs"`
	ExercisePush           string     `json:"exercise_push"`
	ExercisePull           string     `json:"exercise_pull"`
	Status                 string     `json:"status"`
	InscriptionFeePaid     bool       `json:"inscription_fee_paid"`
	InscriptionPaidAt      *time.Time `json:"inscription_paid_at"`
	InscriptionRefundedAt  *time.Time `json:"inscription_refunded_at"`
	DisqualificationReason string     `json:"disqualification_reason"`
	DisqualifiedAt         *time.Time `json:"disqualified_at"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

type measurementResp struct {
	ID              uuid.UUID  `json:"id"`
	ParticipantID   uuid.UUID  `json:"participant_id"`
	Moment          string     `json:"moment"`
	MeasuredAt      time.Time  `json:"measured_at"`
	BodyWeightKg    float64    `json:"body_weight_kg"`
	BodyFatPct      float64    `json:"body_fat_pct"`
	LegsWeightKg    float64    `json:"legs_weight_kg"`
	LegsReps        int        `json:"legs_reps"`
	PushWeightKg    float64    `json:"push_weight_kg"`
	PushReps        int        `json:"push_reps"`
	PullWeightKg    float64    `json:"pull_weight_kg"`
	PullReps        int        `json:"pull_reps"`
	Notes           string     `json:"notes"`
	CreatedByUserID uuid.UUID  `json:"created_by_user_id"`
	SupersededAt    *time.Time `json:"superseded_at"`
	SupersededByID  *uuid.UUID `json:"superseded_by_id"`
	CreatedAt       time.Time  `json:"created_at"`
}

type rankingEntryResp struct {
	ParticipantID          uuid.UUID `json:"participant_id"`
	CategoryID             uuid.UUID `json:"category_id"`
	MemberID               uuid.UUID `json:"member_id"`
	IR                     float64   `json:"ir"`
	DeltaFatPct            float64   `json:"delta_fat_pct"`
	DeltaMusclePct         float64   `json:"delta_muscle_pct"`
	DeltaStrengthPct       float64   `json:"delta_strength_pct"`
	Position               int       `json:"position"`
	Tied                   bool      `json:"tied"`
	AttendanceInsufficient bool      `json:"attendance_insufficient"`
}

// ─── handlers — list/detail/create/update/transition ──────────────────────

func (ctrl *ChallengeController) handleList(c *gin.Context) {
	if ctrl.ListChallenges == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	in := challengesApp.ListChallengesInput{
		GymID:    gymID,
		Status:   c.Query("status"),
		Page:     parseInt(c.Query("page"), 1),
		PageSize: parseInt(c.Query("page_size"), 20),
	}
	out, err := ctrl.ListChallenges.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]challengeResp, len(out.Items))
	for i, ch := range out.Items {
		items[i] = toResp(ch)
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"items":     items,
		"total":     out.Total,
		"page":      out.Page,
		"page_size": out.PageSize,
	})
}

func (ctrl *ChallengeController) handleDetail(c *gin.Context) {
	if ctrl.GetChallengeDetail == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.GetChallengeDetail.Execute(c.Request.Context(), challengesApp.GetChallengeDetailInput{
		GymID: gymID, ChallengeID: id,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	cats := make([]categoryResp, len(out.Categories))
	for i, cat := range out.Categories {
		cats[i] = toCategoryResp(cat)
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"challenge":         toResp(out.Challenge),
		"categories":        cats,
		"participant_count": out.ParticipantCount,
		"t0_captured":       out.T0Captured,
		"t1_captured":       out.T1Captured,
	})
}

func (ctrl *ChallengeController) handleCreate(c *gin.Context) {
	if ctrl.CreateChallenge == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	var req createChallengeReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.CreateChallenge.Execute(c.Request.Context(), challengesApp.CreateChallengeInput{
		GymID:                 gymID,
		ActorUserID:           userID,
		Name:                  req.Name,
		Description:           req.Description,
		StartsAt:              req.StartsAt,
		MeasurementT0Deadline: req.MeasurementT0Deadline,
		MeasurementT1Start:    req.MeasurementT1Start,
		EndsAt:                req.EndsAt,
		InscriptionFeeCents:   req.InscriptionFeeCents,
		InscriptionRefundable: req.InscriptionRefundable,
		MinWeeklyAttendance:   req.MinWeeklyAttendance,
		AttendanceGraceWeeks:  req.AttendanceGraceWeeks,
		StrengthCapPct:        req.StrengthCapPct,
		TieMarginIR:           req.TieMarginIR,
		BFFloorMalePct:        req.BFFloorMalePct,
		BFFloorFemalePct:      req.BFFloorFemalePct,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, toResp(out))
}

func (ctrl *ChallengeController) handleUpdateConfig(c *gin.Context) {
	if ctrl.UpdateChallengeConfig == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req updateChallengeReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.UpdateChallengeConfig.Execute(c.Request.Context(), challengesApp.UpdateChallengeConfigInput{
		GymID:       gymID,
		ActorUserID: userID,
		ChallengeID: id,
		Config: challengeDomain.ConfigUpdate{
			Name:                  req.Name,
			Description:           req.Description,
			StartsAt:              req.StartsAt,
			MeasurementT0Deadline: req.MeasurementT0Deadline,
			MeasurementT1Start:    req.MeasurementT1Start,
			EndsAt:                req.EndsAt,
			InscriptionFeeCents:   req.InscriptionFeeCents,
			InscriptionRefundable: req.InscriptionRefundable,
			MinWeeklyAttendance:   req.MinWeeklyAttendance,
			AttendanceGraceWeeks:  req.AttendanceGraceWeeks,
			StrengthCapPct:        req.StrengthCapPct,
			TieMarginIR:           req.TieMarginIR,
			BFFloorMalePct:        req.BFFloorMalePct,
			BFFloorFemalePct:      req.BFFloorFemalePct,
		},
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toResp(out))
}

func (ctrl *ChallengeController) handleTransition(c *gin.Context) {
	if ctrl.TransitionStatus == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req transitionReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.TransitionStatus.Execute(c.Request.Context(), challengesApp.TransitionChallengeStatusInput{
		GymID: gymID, ActorUserID: userID,
		ChallengeID: id, Transition: req.Transition,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toResp(out))
}

// ─── handlers — categories ────────────────────────────────────────────────

func (ctrl *ChallengeController) handleListCategories(c *gin.Context) {
	if ctrl.ListCategories == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.ListCategories.Execute(c.Request.Context(), challengesApp.ListCategoriesInput{
		GymID: gymID, ChallengeID: id,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := make([]categoryResp, len(out))
	for i, cat := range out {
		resp[i] = toCategoryResp(cat)
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": resp})
}

func (ctrl *ChallengeController) handleAddCategory(c *gin.Context) {
	if ctrl.AddCategory == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req categoryReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.AddCategory.Execute(c.Request.Context(), challengesApp.AddCategoryInput{
		GymID: gymID, ActorUserID: userID, ChallengeID: id,
		Name: req.Name, SortOrder: req.SortOrder,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, toCategoryResp(out))
}

func (ctrl *ChallengeController) handleUpdateCategory(c *gin.Context) {
	if ctrl.UpdateCategory == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	catID, ok := parseUUIDParam(c, "catId")
	if !ok {
		return
	}
	var req updateCategoryReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.UpdateCategory.Execute(c.Request.Context(), challengesApp.UpdateCategoryInput{
		GymID: gymID, ActorUserID: userID, ChallengeID: id, CategoryID: catID,
		Name: req.Name, SortOrder: req.SortOrder,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toCategoryResp(out))
}

func (ctrl *ChallengeController) handleDeleteCategory(c *gin.Context) {
	if ctrl.DeleteCategory == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	catID, ok := parseUUIDParam(c, "catId")
	if !ok {
		return
	}
	if err := ctrl.DeleteCategory.Execute(c.Request.Context(), challengesApp.DeleteCategoryInput{
		GymID: gymID, ActorUserID: userID, ChallengeID: id, CategoryID: catID,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ─── handlers — participants ──────────────────────────────────────────────

func (ctrl *ChallengeController) handleListParticipants(c *gin.Context) {
	if ctrl.ListParticipants == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var catFilter *uuid.UUID
	if q := c.Query("category_id"); q != "" {
		if cid, err := uuid.Parse(q); err == nil {
			catFilter = &cid
		}
	}
	out, err := ctrl.ListParticipants.Execute(c.Request.Context(), challengesApp.ListParticipantsInput{
		GymID: gymID, ChallengeID: id,
		Status:     c.Query("status"),
		CategoryID: catFilter,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := make([]participantResp, len(out))
	for i, p := range out {
		resp[i] = toParticipantResp(p)
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": resp})
}

func (ctrl *ChallengeController) handleAddParticipant(c *gin.Context) {
	if ctrl.AddParticipant == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req addParticipantReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.AddParticipant.Execute(c.Request.Context(), challengesApp.AddParticipantInput{
		GymID: gymID, ActorUserID: userID, ChallengeID: id,
		MemberID: req.MemberID, CategoryID: req.CategoryID,
		Exercises: participantDomain.ExerciseSelection{
			Legs: req.ExerciseLegs,
			Push: req.ExercisePush,
			Pull: req.ExercisePull,
		},
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, toParticipantResp(out))
}

func (ctrl *ChallengeController) handleUpdateParticipant(c *gin.Context) {
	if ctrl.UpdateParticipant == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	pid, ok := parseUUIDParam(c, "pid")
	if !ok {
		return
	}
	var req updateParticipantReq
	if !bind(c, &req) {
		return
	}
	in := challengesApp.UpdateParticipantInput{
		GymID: gymID, ActorUserID: userID,
		ChallengeID: id, ParticipantID: pid,
		MarkFeePaid: req.MarkFeePaid,
		Withdraw:    req.Withdraw,
	}
	if req.ExerciseLegs != nil || req.ExercisePush != nil || req.ExercisePull != nil {
		// Treat partial exercise edits as a full replacement — the client
		// is expected to send the complete trio when changing any.
		ex := participantDomain.ExerciseSelection{}
		if req.ExerciseLegs != nil {
			ex.Legs = *req.ExerciseLegs
		}
		if req.ExercisePush != nil {
			ex.Push = *req.ExercisePush
		}
		if req.ExercisePull != nil {
			ex.Pull = *req.ExercisePull
		}
		in.Exercises = &ex
	}
	out, err := ctrl.UpdateParticipant.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toParticipantResp(out))
}

func (ctrl *ChallengeController) handleRemoveParticipant(c *gin.Context) {
	if ctrl.RemoveParticipant == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	pid, ok := parseUUIDParam(c, "pid")
	if !ok {
		return
	}
	if err := ctrl.RemoveParticipant.Execute(c.Request.Context(), challengesApp.RemoveParticipantInput{
		GymID: gymID, ActorUserID: userID, ChallengeID: id, ParticipantID: pid,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ─── handlers — measurements ──────────────────────────────────────────────

func (ctrl *ChallengeController) handleListMeasurements(c *gin.Context) {
	if ctrl.ListMeasurements == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	pid, ok := parseUUIDParam(c, "pid")
	if !ok {
		return
	}
	out, err := ctrl.ListMeasurements.Execute(c.Request.Context(), challengesApp.ListMeasurementsInput{
		GymID: gymID, ChallengeID: id, ParticipantID: pid,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := make([]measurementResp, len(out))
	for i, m := range out {
		resp[i] = toMeasurementResp(m)
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": resp})
}

func (ctrl *ChallengeController) handleCaptureMeasurement(c *gin.Context) {
	if ctrl.CaptureMeasurement == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	pid, ok := parseUUIDParam(c, "pid")
	if !ok {
		return
	}
	var req captureMeasurementReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.CaptureMeasurement.Execute(c.Request.Context(), challengesApp.CaptureMeasurementInput{
		GymID: gymID, ActorUserID: userID, ChallengeID: id, ParticipantID: pid,
		Input: measurementDomain.Input{
			Moment:          req.Moment,
			MeasuredAt:      req.MeasuredAt,
			BodyWeightKg:    req.BodyWeightKg,
			BodyFatPct:      req.BodyFatPct,
			LegsWeightKg:    req.LegsWeightKg,
			LegsReps:        req.LegsReps,
			PushWeightKg:    req.PushWeightKg,
			PushReps:        req.PushReps,
			PullWeightKg:    req.PullWeightKg,
			PullReps:        req.PullReps,
			Notes:           req.Notes,
			CreatedByUserID: userID,
		},
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, gin.H{
		"measurement":         toMeasurementResp(out.Measurement),
		"superseded_prior_id": out.SupersededPriorID,
		"participant_status":  out.ParticipantStatus,
	})
}

// ─── handlers — ranking / attendance / DQ ─────────────────────────────────

func (ctrl *ChallengeController) handleRanking(c *gin.Context) {
	if ctrl.GetChallengeRanking == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var catFilter *uuid.UUID
	if q := c.Query("category_id"); q != "" {
		if cid, err := uuid.Parse(q); err == nil {
			catFilter = &cid
		}
	}
	out, err := ctrl.GetChallengeRanking.Execute(c.Request.Context(), challengesApp.GetChallengeRankingInput{
		GymID: gymID, ChallengeID: id, CategoryID: catFilter,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := make([]rankingEntryResp, len(out))
	for i, e := range out {
		resp[i] = rankingEntryResp{
			ParticipantID:          e.ParticipantID,
			CategoryID:             e.CategoryID,
			MemberID:               e.MemberID,
			IR:                     e.IR,
			DeltaFatPct:            e.Score.DeltaFatPct,
			DeltaMusclePct:         e.Score.DeltaMusclePct,
			DeltaStrengthPct:       e.Score.DeltaStrengthPct,
			Position:               e.Position,
			Tied:                   e.Tied,
			AttendanceInsufficient: e.AttendanceInsufficient,
		}
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": resp})
}

func (ctrl *ChallengeController) handleAttendance(c *gin.Context) {
	if ctrl.GetAttendanceReport == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.GetAttendanceReport.Execute(c.Request.Context(), challengesApp.GetAttendanceReportInput{
		GymID: gymID, ChallengeID: id,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": out})
}

func (ctrl *ChallengeController) handleCheckDisqualifications(c *gin.Context) {
	if ctrl.CheckDisqualifications == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.CheckDisqualifications.Execute(c.Request.Context(), challengesApp.CheckDisqualificationsInput{
		GymID: gymID, ActorUserID: userID, ChallengeID: id,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"disqualified_ids": out.DisqualifiedIDs,
		"count":            len(out.DisqualifiedIDs),
	})
}
