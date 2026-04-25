package controllers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// MemberController bundles UC-012..UC-017 + UC-032 partial.
type MemberController struct {
	Create     *memApp.CreateMember
	Update     *memApp.UpdateMember
	List       *memApp.ListMembers
	Detail     *memApp.GetMemberDetail
	Toggle     *memApp.ToggleMemberStatus
	LockExpiry *memApp.LockMembershipExpiry
	AssignPin  *memApp.AssignPin
	Tokens     auth.TokenService
}

func NewMemberController(
	create *memApp.CreateMember,
	update *memApp.UpdateMember,
	list *memApp.ListMembers,
	detail *memApp.GetMemberDetail,
	toggle *memApp.ToggleMemberStatus,
	lockExpiry *memApp.LockMembershipExpiry,
	assignPin *memApp.AssignPin,
	tokens auth.TokenService,
) *MemberController {
	return &MemberController{
		Create: create, Update: update, List: list, Detail: detail,
		Toggle: toggle, LockExpiry: lockExpiry, AssignPin: assignPin, Tokens: tokens,
	}
}

func (ctrl *MemberController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		members := api.Group("/members")
		members.POST("", ctrl.handleCreate)
		members.GET("", ctrl.handleList)
		members.GET("/:id", ctrl.handleDetail)
		members.PATCH("/:id", ctrl.handleUpdate)
		members.PATCH("/:id/status", ctrl.handleToggleStatus)
		members.POST("/:id/pin", ctrl.handleAssignPin)

		// UC-017 — only owner can adjust expiry (DA-17.1).
		api.POST("/memberships/:id/lock-expiry", middleware.RequireOwner(), ctrl.handleLockExpiry)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type createMemberReq struct {
	FullName            string  `json:"full_name" validate:"required,min=3,max=100"`
	Phone               string  `json:"phone" validate:"required"`
	Email               *string `json:"email,omitempty"`
	Birthdate           *string `json:"birthdate,omitempty"` // YYYY-MM-DD
	PhotoURL            *string `json:"photo_url,omitempty"`
	Notes               *string `json:"notes,omitempty"`
	MembershipTypeID    string  `json:"membership_type_id" validate:"required,uuid"`
	StartDate           string  `json:"start_date,omitempty"` // YYYY-MM-DD; defaults to today
	AllowDuplicatePhone bool    `json:"allow_duplicate_phone,omitempty"`
	ChargeFirstPayment  bool    `json:"charge_first_payment,omitempty"`
}

type createMemberResp struct {
	MemberID            uuid.UUID `json:"member_id"`
	MembershipID        uuid.UUID `json:"membership_id"`
	Folio               string    `json:"folio"`
	ExpiryDate          string    `json:"expiry_date"`
	PendingFirstPayment bool      `json:"pending_first_payment"`
}

type updateMemberReq struct {
	FullName  *string `json:"full_name,omitempty"`
	Phone     *string `json:"phone,omitempty"`
	Email     *string `json:"email,omitempty"`
	Birthdate *string `json:"birthdate,omitempty"`
	PhotoURL  *string `json:"photo_url,omitempty"`
	Notes     *string `json:"notes,omitempty"`
}

type toggleStatusReq struct {
	Status string `json:"status" validate:"required,oneof=active inactive lost"`
	Reason string `json:"reason,omitempty"`
}

type lockExpiryReq struct {
	Mode      string `json:"mode" validate:"required,oneof=extend set"`
	Days      int    `json:"days,omitempty"`
	NewExpiry string `json:"new_expiry,omitempty"` // YYYY-MM-DD when mode=set
	Reason    string `json:"reason" validate:"required,min=5,max=500"`
}

type assignPinReq struct {
	Pin string `json:"pin,omitempty"` // optional — empty = auto-generate
}

type memberResp struct {
	ID                   uuid.UUID  `json:"id"`
	GymID                uuid.UUID  `json:"gym_id"`
	Folio                string     `json:"folio"`
	FullName             string     `json:"full_name"`
	Phone                string     `json:"phone"`
	Email                *string    `json:"email,omitempty"`
	Birthdate            *string    `json:"birthdate,omitempty"`
	PhotoURL             *string    `json:"photo_url,omitempty"`
	Notes                *string    `json:"notes,omitempty"`
	Status               string     `json:"status"`
	EnrollmentPaid       bool       `json:"enrollment_paid"`
	LastMaintenancePaid  *string    `json:"last_maintenance_paid,omitempty"`
	HasPin               bool       `json:"has_pin"`
	LastContactAttemptAt *time.Time `json:"last_contact_attempt_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

type membershipResp struct {
	ID                   uuid.UUID `json:"id"`
	MembershipTypeID     uuid.UUID `json:"membership_type_id"`
	TypeName             string    `json:"type_name"`
	Price                float64   `json:"price"`
	StartDate            string    `json:"start_date"`
	ExpiryDate           string    `json:"expiry_date"`
	Status               string    `json:"status"`
	DurationDaysSnapshot int       `json:"duration_days_snapshot"`
}

type memberDetailResp struct {
	Member            memberResp      `json:"member"`
	CurrentMembership *membershipResp `json:"current_membership,omitempty"`
	AccessStatus      string          `json:"access_status"`
}

type memberListItemResp struct {
	Member            memberResp      `json:"member"`
	CurrentMembership *membershipResp `json:"current_membership,omitempty"`
	AccessStatus      string          `json:"access_status"`
}

type memberListResp struct {
	Items    []memberListItemResp `json:"items"`
	Total    int                  `json:"total"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *MemberController) handleCreate(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req createMemberReq
	if !bindJSON(c, &req) {
		return
	}
	typeID, err := uuid.Parse(req.MembershipTypeID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	now := time.Now().UTC()
	startDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if req.StartDate != "" {
		t, err := time.Parse("2006-01-02", req.StartDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		startDate = t
	}
	var birthdate *time.Time
	if req.Birthdate != nil && *req.Birthdate != "" {
		t, err := time.Parse("2006-01-02", *req.Birthdate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		birthdate = &t
	}
	out, err := ctrl.Create.Execute(c.Request.Context(), memApp.CreateMemberInput{
		GymID: gymID, ActorUserID: userID,
		FullName:            req.FullName,
		Phone:               req.Phone,
		Email:               req.Email,
		Birthdate:           birthdate,
		PhotoURL:            req.PhotoURL,
		Notes:               req.Notes,
		MembershipTypeID:    typeID,
		StartDate:           startDate,
		AllowDuplicatePhone: req.AllowDuplicatePhone,
		ChargeFirstPayment:  req.ChargeFirstPayment,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, createMemberResp{
		MemberID:            out.MemberID,
		MembershipID:        out.MembershipID,
		Folio:               out.Folio,
		ExpiryDate:          out.ExpiryDate.Format("2006-01-02"),
		PendingFirstPayment: out.PendingFirstPayment,
	})
}

func (ctrl *MemberController) handleUpdate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req updateMemberReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	var birthdate *time.Time
	if req.Birthdate != nil {
		if *req.Birthdate == "" {
			zero := time.Time{}
			birthdate = &zero
		} else {
			t, err := time.Parse("2006-01-02", *req.Birthdate)
			if err != nil {
				utils.ErrorResponse(c, http.StatusBadRequest, err)
				return
			}
			birthdate = &t
		}
	}
	out, err := ctrl.Update.Execute(c.Request.Context(), memApp.UpdateMemberInput{
		GymID: gymID, ActorUserID: userID, MemberID: id,
		FullName: req.FullName, Phone: req.Phone, Email: req.Email,
		Birthdate: birthdate, PhotoURL: req.PhotoURL, Notes: req.Notes,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toMemberResp(out))
}

func (ctrl *MemberController) handleList(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "25"))
	sort := c.Query("sort")
	asc := c.Query("dir") != "desc"
	var planID *uuid.UUID
	if v := c.Query("plan_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			planID = &id
		}
	}
	out, err := ctrl.List.Execute(c.Request.Context(), memApp.ListMembersInput{
		GymID: gymID, Search: c.Query("q"), StatusFilter: c.Query("status"),
		PlanID: planID, Sort: sort, SortAscending: asc,
		Page: page, PageSize: pageSize,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]memberListItemResp, 0, len(out.Items))
	for _, it := range out.Items {
		items = append(items, memberListItemResp{
			Member:            toMemberResp(it.Member),
			CurrentMembership: toMembershipResp(it.CurrentMembership),
			AccessStatus:      string(it.AccessStatus),
		})
	}
	utils.JsonResponse(c, http.StatusOK, memberListResp{
		Items: items, Total: out.Total, Page: out.Page, PageSize: out.PageSize,
	})
}

func (ctrl *MemberController) handleDetail(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.Detail.Execute(c.Request.Context(), memApp.GetMemberDetailInput{GymID: gymID, MemberID: id})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, memberDetailResp{
		Member:            toMemberResp(out.Member),
		CurrentMembership: toMembershipResp(out.CurrentMembership),
		AccessStatus:      string(out.AccessStatus),
	})
}

func (ctrl *MemberController) handleToggleStatus(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req toggleStatusReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.Toggle.Execute(c.Request.Context(), memApp.ToggleMemberStatusInput{
		GymID: gymID, ActorUserID: userID, MemberID: id,
		NewStatus: req.Status, Reason: req.Reason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toMemberResp(out))
}

func (ctrl *MemberController) handleLockExpiry(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req lockExpiryReq
	if !bindJSON(c, &req) {
		return
	}
	in := memApp.LockMembershipExpiryInput{
		GymID: gymID, ActorUserID: userID, MembershipID: id,
		Reason: req.Reason,
	}
	switch req.Mode {
	case "extend":
		in.Mode = memApp.ModeExtendDays
		in.Days = req.Days
	case "set":
		in.Mode = memApp.ModeSetDate
		t, err := time.Parse("2006-01-02", req.NewExpiry)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		in.NewExpiry = t
	}
	out, err := ctrl.LockExpiry.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"membership_id":   out.MembershipID,
		"previous_expiry": out.PreviousExpiry.Format("2006-01-02"),
		"new_expiry":      out.NewExpiry.Format("2006-01-02"),
		"days_added":      out.DaysAdded,
	})
}

func (ctrl *MemberController) handleAssignPin(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req assignPinReq
	_ = c.ShouldBindJSON(&req)
	out, err := ctrl.AssignPin.Execute(c.Request.Context(), memApp.AssignPinInput{
		GymID: gymID, ActorUserID: userID, MemberID: id, PlainPin: req.Pin,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"member_id": out.MemberID, "pin": out.Pin})
}

// ---------------------------------------------------------------------------
// Wire mappers
// ---------------------------------------------------------------------------

func toMemberResp(m *memberDomain.Member) memberResp {
	if m == nil {
		return memberResp{}
	}
	r := memberResp{
		ID:                   m.ID,
		GymID:                m.GymID,
		Folio:                m.Folio,
		FullName:             m.FullName,
		Phone:                m.Phone,
		Email:                m.Email,
		PhotoURL:             m.PhotoURL,
		Notes:                m.Notes,
		Status:               m.Status,
		EnrollmentPaid:       m.EnrollmentPaid,
		HasPin:               m.PinHash != nil,
		LastContactAttemptAt: m.LastContactAttemptAt,
		CreatedAt:            m.CreatedAt,
	}
	if m.Birthdate != nil {
		s := m.Birthdate.Format("2006-01-02")
		r.Birthdate = &s
	}
	if m.LastMaintenancePaid != nil {
		s := m.LastMaintenancePaid.Format("2006-01-02")
		r.LastMaintenancePaid = &s
	}
	return r
}

func toMembershipResp(ms *membershipDomain.Membership) *membershipResp {
	if ms == nil {
		return nil
	}
	return &membershipResp{
		ID:                   ms.ID,
		MembershipTypeID:     ms.MembershipTypeID,
		TypeName:             ms.TypeNameSnapshot,
		Price:                ms.PriceSnapshot,
		StartDate:            ms.StartDate.Format("2006-01-02"),
		ExpiryDate:           ms.ExpiryDate.Format("2006-01-02"),
		Status:               ms.Status,
		DurationDaysSnapshot: ms.DurationDaysSnapshot,
	}
}

// access enum re-export so callers compiling outside this package don't import members/domain/access.
var _ access.AccessStatus = access.AllowedActive
