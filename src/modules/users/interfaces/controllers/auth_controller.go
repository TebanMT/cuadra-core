// Package controllers exposes HTTP handlers for users + auth (UC-001..UC-004,
// UC-006..UC-009). Owner-only endpoints sit behind RequireOwner; the rest
// behind AuthMiddleware. Routes are bound in RegisterRoutes.
package controllers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	gymApp "github.com/cuadra/cuadra-core/src/modules/gyms/app"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// AuthController bundles every UC that touches identity. It also owns the
// gym-side wizard endpoints since they share the same lifecycle.
type AuthController struct {
	Signup           *usersApp.SignupOwner
	Login            *usersApp.Login
	Logout           *usersApp.Logout
	RequestReset     *usersApp.RequestPasswordReset
	ConfirmReset     *usersApp.ConfirmPasswordReset
	UpdateBasicInfo  *gymApp.UpdateBasicInfo
	UpdatePayMethods *gymApp.UpdatePaymentMethods
	CompleteSetup    *gymApp.CompleteSetup
	UpdateProfile    *gymApp.UpdateProfile
	CreateOperator   *usersApp.CreateOperator
	UpdateOperator   *usersApp.UpdateOperator
	ToggleActive     *usersApp.ToggleOperatorActive
	ResetOpPassword  *usersApp.ResetOperatorPassword
	RequestTransfer  *usersApp.RequestTransferOwnership
	ConfirmTransfer  *usersApp.ConfirmTransferOwnership
	Tokens           auth.TokenService
}

func NewAuthController(deps AuthController) *AuthController { c := deps; return &c }

func (ctrl *AuthController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	authGrp := api.Group("/auth")
	{
		authGrp.POST("/signup", ctrl.handleSignup)
		authGrp.POST("/login", ctrl.handleLogin)
		authGrp.POST("/logout", ctrl.handleLogout)
		authGrp.POST("/forgot-password", ctrl.handleForgotPassword)
		authGrp.POST("/reset-password", ctrl.handleResetPassword)
	}

	gyms := api.Group("/gyms")
	gyms.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		gyms.PATCH("/me/setup", ctrl.handleUpdateSetup)                  // step 2
		gyms.POST("/me/setup/complete", ctrl.handleCompleteSetup)        // step 5
		gyms.PATCH("/me/payment-methods", ctrl.handleUpdatePaymentMeths) // step 4
		gyms.PATCH("/me", middleware.RequireOwner(), ctrl.handleUpdateProfile)
		gyms.POST("/me/transfer-ownership", middleware.RequireOwner(), ctrl.handleTransferOwnership)
	}

	users := api.Group("/users")
	users.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		users.POST("", middleware.RequireOwner(), ctrl.handleCreateOperator)
		users.PATCH("/:id", middleware.RequireOwner(), ctrl.handleUpdateOperator)
		users.PATCH("/:id/active", middleware.RequireOwner(), ctrl.handleToggleActive)
		users.POST("/:id/reset-password", middleware.RequireOwner(), ctrl.handleResetOpPassword)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type signupReq struct {
	FullName        string `json:"full_name" validate:"required,min=3,max=100"`
	Email           string `json:"email" validate:"required,email"`
	Password        string `json:"password" validate:"required,min=8"`
	PasswordConfirm string `json:"password_confirm" validate:"required"`
}

type signupResp struct {
	UserID         uuid.UUID `json:"user_id"`
	GymID          uuid.UUID `json:"gym_id"`
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	SetupCompleted bool      `json:"setup_completed"`
}

type loginReq struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

type loginResp struct {
	UserID             uuid.UUID  `json:"user_id"`
	GymID              uuid.UUID  `json:"gym_id"`
	Role               string     `json:"role"`
	AccessToken        string     `json:"access_token"`
	RefreshToken       string     `json:"refresh_token"`
	SetupCompleted     bool       `json:"setup_completed"`
	TrialEndsAt        *time.Time `json:"trial_ends_at,omitempty"`
	SubscriptionPlan   string     `json:"subscription_plan"`
	MustChangePassword bool       `json:"must_change_password"`
}

type logoutReq struct {
	RefreshToken string `json:"refresh_token"`
}

type forgotPasswordReq struct {
	Email string `json:"email" validate:"required,email"`
}

type resetPasswordReq struct {
	Token       string `json:"token" validate:"required"`
	NewPassword string `json:"new_password" validate:"required,min=8"`
}

type updateSetupReq struct {
	Name     string `json:"name" validate:"required,min=1,max=100"`
	City     string `json:"city"`
	WhatsApp string `json:"whatsapp"`
}

type updatePaymentMethodsReq struct {
	Methods []string `json:"methods" validate:"required,min=1,dive,oneof=cash transfer card"`
}

type updateGymProfileReq struct {
	Name           *string `json:"name,omitempty"`
	City           *string `json:"city,omitempty"`
	WhatsApp       *string `json:"whatsapp,omitempty"`
	Timezone       *string `json:"timezone,omitempty"`
	RFC            *string `json:"rfc,omitempty"`
	RazonSocial    *string `json:"razon_social,omitempty"`
	CodigoPostal   *string `json:"codigo_postal,omitempty"`
	RegimenFiscal  *string `json:"regimen_fiscal,omitempty"`
	LogoURL        *string `json:"logo_url,omitempty"`
	PrimaryColor   *string `json:"primary_color,omitempty"`
	SecondaryColor *string `json:"secondary_color,omitempty"`
	OpenTime       *string `json:"open_time,omitempty"`
	CloseTime      *string `json:"close_time,omitempty"`
}

type createOperatorReq struct {
	FullName string `json:"full_name" validate:"required,min=3,max=100"`
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password,omitempty"` // optional; empty -> auto-generated
}

type createOperatorResp struct {
	UserID   uuid.UUID `json:"user_id"`
	Email    string    `json:"email"`
	Password string    `json:"password"` // shown ONCE; the wizard tells the user to copy
}

type updateOperatorReq struct {
	FullName *string `json:"full_name,omitempty"`
	Email    *string `json:"email,omitempty"`
	Phone    *string `json:"phone,omitempty"`
}

type toggleActiveReq struct {
	Active bool `json:"active"`
}

type transferReq struct {
	TargetUserID uuid.UUID `json:"target_user_id" validate:"required"`
	Code         string    `json:"code,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers — wide and dumb on purpose. All business logic lives in app/.
// ---------------------------------------------------------------------------

func (ctrl *AuthController) handleSignup(c *gin.Context) {
	var req signupReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.Signup.Execute(c.Request.Context(), usersApp.SignupOwnerInput{
		FullName: req.FullName, Email: req.Email,
		Password: req.Password, PasswordConfirm: req.PasswordConfirm,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, signupResp{
		UserID:         out.UserID,
		GymID:          out.GymID,
		AccessToken:    out.AccessToken,
		RefreshToken:   out.RefreshToken,
		SetupCompleted: out.SetupCompleted,
	})
}

func (ctrl *AuthController) handleLogin(c *gin.Context) {
	var req loginReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.Login.Execute(c.Request.Context(), usersApp.LoginInput{Email: req.Email, Password: req.Password})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, loginResp{
		UserID:             out.UserID,
		GymID:              out.GymID,
		Role:               out.Role,
		AccessToken:        out.AccessToken,
		RefreshToken:       out.RefreshToken,
		SetupCompleted:     out.SetupCompleted,
		TrialEndsAt:        out.TrialEndsAt,
		SubscriptionPlan:   out.SubscriptionPlan,
		MustChangePassword: out.MustChangePassword,
	})
}

func (ctrl *AuthController) handleLogout(c *gin.Context) {
	var req logoutReq
	_ = c.ShouldBindJSON(&req)
	if err := ctrl.Logout.Execute(c.Request.Context(), usersApp.LogoutInput{RefreshToken: req.RefreshToken}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (ctrl *AuthController) handleForgotPassword(c *gin.Context) {
	var req forgotPasswordReq
	if !bind(c, &req) {
		return
	}
	if err := ctrl.RequestReset.Execute(c.Request.Context(), req.Email); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	// Always 200 regardless of whether the email exists — UC-004.
	utils.JsonResponse(c, http.StatusOK, gin.H{"sent": true})
}

func (ctrl *AuthController) handleResetPassword(c *gin.Context) {
	var req resetPasswordReq
	if !bind(c, &req) {
		return
	}
	if err := ctrl.ConfirmReset.Execute(c.Request.Context(), req.Token, req.NewPassword); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"ok": true})
}

func (ctrl *AuthController) handleUpdateSetup(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req updateSetupReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.UpdateBasicInfo.Execute(c.Request.Context(), gymApp.UpdateBasicInfoInput{
		GymID: gymID, ActorUserID: userID,
		Name: req.Name, City: req.City, WhatsApp: req.WhatsApp,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toGymResponse(out))
}

func (ctrl *AuthController) handleCompleteSetup(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	out, err := ctrl.CompleteSetup.Execute(c.Request.Context(), gymID, userID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toGymResponse(out))
}

func (ctrl *AuthController) handleUpdatePaymentMeths(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	var req updatePaymentMethodsReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.UpdatePayMethods.Execute(c.Request.Context(), gymApp.UpdatePaymentMethodsInput{
		GymID: gymID, ActorUserID: userID, Methods: req.Methods,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toGymResponse(out))
}

func (ctrl *AuthController) handleUpdateProfile(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	var req updateGymProfileReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	out, err := ctrl.UpdateProfile.Execute(c.Request.Context(), gymApp.UpdateProfileInput{
		GymID: gymID, ActorUserID: userID,
		Update: gymDomain.ProfileUpdate{
			Name: req.Name, City: req.City, WhatsApp: req.WhatsApp,
			Timezone: req.Timezone, RFC: req.RFC, RazonSocial: req.RazonSocial,
			CodigoPostal: req.CodigoPostal, RegimenFiscal: req.RegimenFiscal,
			LogoURL: req.LogoURL, PrimaryColor: req.PrimaryColor, SecondaryColor: req.SecondaryColor,
			OpenTime: req.OpenTime, CloseTime: req.CloseTime,
		},
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toGymResponse(out))
}

func (ctrl *AuthController) handleCreateOperator(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	ownerID, _ := middleware.GetUserID(c)
	var req createOperatorReq
	if !bind(c, &req) {
		return
	}
	out, err := ctrl.CreateOperator.Execute(c.Request.Context(), usersApp.CreateOperatorInput{
		GymID: gymID, OwnerID: ownerID,
		FullName: req.FullName, Email: req.Email, Password: req.Password,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, createOperatorResp{
		UserID: out.UserID, Email: out.Email, Password: out.Password,
	})
}

func (ctrl *AuthController) handleUpdateOperator(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	targetID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req updateOperatorReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	out, err := ctrl.UpdateOperator.Execute(c.Request.Context(), usersApp.UpdateOperatorInput{
		GymID: gymID, ActorUserID: actorID, TargetID: targetID,
		FullName: req.FullName, Email: req.Email, Phone: req.Phone,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toUserResponse(out))
}

func (ctrl *AuthController) handleToggleActive(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	targetID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req toggleActiveReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	out, err := ctrl.ToggleActive.Execute(c.Request.Context(), usersApp.ToggleOperatorActiveInput{
		GymID: gymID, ActorUserID: actorID, TargetID: targetID, Active: req.Active,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toUserResponse(out))
}

func (ctrl *AuthController) handleResetOpPassword(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	targetID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.ResetOpPassword.Execute(c.Request.Context(), usersApp.ResetOperatorPasswordInput{
		GymID: gymID, ActorUserID: actorID, TargetID: targetID,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"user_id": out.UserID, "password": out.Password})
}

// handleTransferOwnership covers UC-010's two-step flow on a single endpoint:
// no Code → request OTP; with Code → confirm.
func (ctrl *AuthController) handleTransferOwnership(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	ownerID, _ := middleware.GetUserID(c)
	var req transferReq
	if !bind(c, &req) {
		return
	}
	ctx := c.Request.Context()
	if req.Code == "" {
		if err := ctrl.RequestTransfer.Execute(ctx, usersApp.RequestTransferOwnershipInput{
			GymID: gymID, OwnerID: ownerID, TargetID: req.TargetUserID,
		}); err != nil {
			utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
			return
		}
		utils.JsonResponse(c, http.StatusOK, gin.H{"otp_sent": true})
		return
	}
	out, err := ctrl.ConfirmTransfer.Execute(ctx, usersApp.ConfirmTransferOwnershipInput{
		GymID: gymID, OwnerID: ownerID, Code: req.Code,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"new_owner_id": out.NewOwnerID,
		"transfer_id":  out.TransferID,
		"executed_at":  out.ExecutedAt,
	})
}
