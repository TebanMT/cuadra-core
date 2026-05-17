// Package controllers exposes HTTP handlers for users + auth (UC-001..UC-004,
// UC-006..UC-009). Owner-only endpoints sit behind RequireOwner; the rest
// behind AuthMiddleware. Routes are bound in RegisterRoutes.
package controllers

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	gymApp "github.com/cuadra/cuadra-core/src/modules/gyms/app"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/installerbootstrap"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/r2"
	"github.com/cuadra/cuadra-core/src/shared/sidecartoken"
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
	UpdateChargeSet  *gymApp.UpdateChargeSettings
	CreateOperator    *usersApp.CreateOperator
	UpdateOperator    *usersApp.UpdateOperator
	ToggleActive      *usersApp.ToggleOperatorActive
	ResetOpPassword   *usersApp.ResetOperatorPassword
	RotateOperatorPIN *usersApp.RotateOperatorPIN
	RequestTransfer  *usersApp.RequestTransferOwnership
	ConfirmTransfer  *usersApp.ConfirmTransferOwnership
	Tokens           auth.TokenService
	// Read deps used by me_controller.go for GET /auth/me, GET /gyms/me, and
	// GET /gyms/me/setup-status. They're optional in the sense that nil-safe
	// guards in the handlers turn them into 500s — main.go is expected to
	// wire them.
	Gyms            gymRepo.GymRepository
	Users           usersRepo.UserRepository
	MembershipTypes membershipTypeReader
	UoW             sharedDomain.UnitOfWork
	// UploadsDir is the on-disk root for asset uploads (gym logo today).
	// Empty disables the feature: /me/logo returns 500 and
	// RegisterUploadsRoute is a no-op.
	UploadsDir string
	// SidecarBootstrap mints a sk_live_* credential for sidecar callers
	// (identified by X-Cuadra-Client-ID). Optional — when nil, login/signup
	// responses simply omit the sidecar_token field.
	SidecarBootstrap *usersApp.BootstrapSidecarToken
	// AssignSelfPIN + ClearSelfPIN power POST/DELETE /api/v1/auth/me/pin.
	// Optional — when nil the routes 501 so older binaries built before the
	// auth-refactor stay compilable.
	AssignSelfPIN *usersApp.AssignSelfPIN
	ClearSelfPIN  *usersApp.ClearSelfPIN
	// InstallerBootstrap mints single-use codes the dashboard hands the
	// owner after web signup. Optional — when nil the endpoint 501s.
	InstallerBootstrap *usersApp.IssueInstallerBootstrap
	// RedeemInstaller swaps a one-time bootstrap code for a full session
	// (operator JWTs + sidecar credential). Optional — when nil the
	// endpoint 501s.
	RedeemInstaller *usersApp.RedeemInstallerBootstrap
	// SidecarTokens reads the sidecar_credentials table for the
	// "dispositivos vinculados" listing surfaced by GET /auth/devices. The
	// model already supports N desktops per gym (unique idx is on
	// (gym_id, client_id) WHERE revoked_at IS NULL); this just exposes the
	// list to the dashboard. Optional — endpoint 501s when nil.
	SidecarTokens sidecartoken.Store
	// R2 backs the /uploads/presign endpoint for member photos
	// (offline-first: sidecar agent presigns + uploads on each sync
	// tick). Optional — when nil (env vars no seteadas) el endpoint
	// 501s y el sidecar deja la foto como data URL local hasta que
	// las credenciales aparezcan.
	R2 *r2.Client
}

// membershipTypeReader is the slim subset of MembershipTypeRepository the
// setup-status handler needs (just a count). Defined here so this controller
// doesn't import the whole members module.
type membershipTypeReader interface {
	CountByGym(tx sharedDomain.Transaction, gymID uuid.UUID) (int, error)
}

// HeaderClientID is the client-supplied UUID identifying a sidecar
// installation across logins. Cloud uses it to look up / mint the active
// sidecar_credentials row (ADR-008 §3.3).
const HeaderClientID = "X-Cuadra-Client-ID"

// HeaderDeviceLabel is an opaque, human-readable hint (hostname, model name)
// shown on the dashboard's "active devices" panel.
const HeaderDeviceLabel = "X-Cuadra-Device-Label"

// readClientID parses the X-Cuadra-Client-ID header. Returns uuid.Nil on
// any error so callers can simply skip sidecar bootstrap when absent.
func readClientID(c *gin.Context) uuid.UUID {
	raw := c.GetHeader(HeaderClientID)
	if raw == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return id
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
		authGrp.POST("/redeem-installer", ctrl.handleRedeemInstaller)
	}

	authedAuth := api.Group("/auth")
	authedAuth.Use(middleware.AuthMiddleware(ctrl.Tokens))
	authedAuth.PATCH("/me", ctrl.handleUpdateMe)
	authedAuth.POST("/me/pin", ctrl.handleAssignSelfPIN)
	authedAuth.DELETE("/me/pin", ctrl.handleClearSelfPIN)
	authedAuth.POST("/installer-token", ctrl.handleIssueInstaller)
	authedAuth.GET("/devices", middleware.RequireOwner(), ctrl.handleListDevices)

	// /uploads/presign — acepta JWT (dashboard, futuro) y sk_live_*
	// (sidecar agent en cada tick). El handler delega el scoping de
	// gym_id al middleware (lo extrae del token).
	if ctrl.SidecarTokens != nil {
		uploads := api.Group("/uploads")
		uploads.Use(middleware.SidecarOrJWTMiddleware(ctrl.Tokens, ctrl.SidecarTokens))
		uploads.POST("/presign", ctrl.handleUploadPresign)
		uploads.POST("/presign-get", ctrl.handleUploadPresignGet)
	}

	// GET /auth/me — aceptamos JWT (dashboard) Y sk_live_* (sidecar
	// pidiendo refresh de identidad desde su Settings page). El handler
	// no cambia; ambos middlewares dejan user_id + gym_id en el context.
	// El resto de /me/* (PATCH, /pin, etc.) sigue requiriendo JWT porque
	// son acciones del operador actual, no del bootstrap user del
	// sidecar.
	if ctrl.SidecarTokens != nil {
		meAuth := api.Group("/auth")
		meAuth.Use(middleware.SidecarOrJWTMiddleware(ctrl.Tokens, ctrl.SidecarTokens))
		meAuth.GET("/me", ctrl.handleGetMe)
	} else {
		// Fallback para builds/test donde SidecarTokens no está cableado.
		authedAuth.GET("/me", ctrl.handleGetMe)
	}

	ctrl.RegisterAccountRoutes(r)
	ctrl.RegisterOperatorRoutes(r)
}

// RegisterMeRoute exposes the read-only `GET /api/v1/auth/me` endpoint.
// Split out so the sidecar can wire it alongside SidecarAuthProxy (which
// owns the rest of /auth/*) — me is intentionally local-only on the
// sidecar so the FE's hydrate flow doesn't depend on cloud connectivity.
func (ctrl *AuthController) RegisterMeRoute(r *gin.Engine) {
	authed := r.Group("/api/v1/auth")
	authed.Use(middleware.AuthMiddleware(ctrl.Tokens))
	authed.GET("/me", ctrl.handleGetMe)
	authed.PATCH("/me", ctrl.handleUpdateMe)
	authed.POST("/me/pin", ctrl.handleAssignSelfPIN)
	authed.DELETE("/me/pin", ctrl.handleClearSelfPIN)
}

// RegisterOperatorRoutes registers the operator-management subset
// (`/api/v1/users/*`). Split out so the sidecar can wire it alongside
// SidecarAuthProxy without re-registering the auth subset.
func (ctrl *AuthController) RegisterOperatorRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	users := api.Group("/users")
	users.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		// GET /api/v1/users — owner-only. La página Operadores del desktop la
		// pega para listar a todo el staff del gym (dueño + recepcionistas)
		// con su estado de activación. Aceptamos `?include_inactive=true|false`
		// (default true: Operadores muestra desactivados con badge gris para
		// que el dueño los reactive en un click). Estaba ausente del registro
		// y la página caía en 404 desde el primer load.
		users.GET("", middleware.RequireOwner(), ctrl.handleListOperators)
		users.POST("", middleware.RequireOwner(), ctrl.handleCreateOperator)
		users.PATCH("/:id", middleware.RequireOwner(), ctrl.handleUpdateOperator)
		users.PATCH("/:id/active", middleware.RequireOwner(), ctrl.handleToggleActive)
		users.POST("/:id/reset-password", middleware.RequireOwner(), ctrl.handleResetOpPassword)
		// rotate-pin sustituye a reset-password como la acción primaria.
		// reset-password se conserva para operadores legacy con email+pwd;
		// rotate-pin genera un PIN nuevo, hashea, audita y dispara WhatsApp.
		users.POST("/:id/rotate-pin", middleware.RequireOwner(), ctrl.handleRotateOperatorPIN)
	}
}

// RegisterAccountRoutes registers the gym-account subset (`/api/v1/gyms/me/*`)
// in isolation. Tests that only exercise the gym profile / logo flow can use
// this without wiring all the operator-management dependencies that
// RegisterRoutes pulls in.
func (ctrl *AuthController) RegisterAccountRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	gyms := api.Group("/gyms")
	gyms.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		gyms.GET("/me", ctrl.handleGetGymProfile)
		gyms.GET("/me/setup-status", ctrl.handleSetupStatus)
		// El wizard inicial (steps 2-5) sólo lo corre el dueño en signup —
		// el usuario recién creado siempre tiene rol owner, así que el
		// guard no rompe el flujo normal. Existe como defensa en
		// profundidad para que un operador no pueda re-disparar el wizard
		// y pisar configuración del gym vía API.
		gyms.PATCH("/me/setup", middleware.RequireOwner(), ctrl.handleUpdateSetup)                  // step 2
		gyms.POST("/me/setup/complete", middleware.RequireOwner(), ctrl.handleCompleteSetup)        // step 5
		gyms.PATCH("/me/payment-methods", middleware.RequireOwner(), ctrl.handleUpdatePaymentMeths) // step 4
		// PATCH /me uses the FE-driven shape (whatsapp_number, legal_name,
		// kiosk_volume, …). The legacy handleUpdateProfile is dead code kept
		// around for direct test callers; the wire handler is the active path.
		gyms.PATCH("/me", middleware.RequireOwner(), ctrl.handleUpdateProfileWire)
		gyms.POST("/me/logo", middleware.RequireOwner(), ctrl.handleUploadLogo)
		gyms.POST("/me/transfer-ownership", middleware.RequireOwner(), ctrl.handleTransferOwnership)
		// Charge settings (cobros a nivel gym: inscripción + mantenimiento).
		// Antes vivía en localStorage del desktop — frágil y per-device.
		gyms.GET("/me/charge-settings", ctrl.handleGetChargeSettings)
		gyms.PATCH("/me/charge-settings", middleware.RequireOwner(), ctrl.handleUpdateChargeSettings)
	}
}

// RegisterUploadsRoute exposes UploadsDir at GET /uploads/* as a public,
// auth-free route so the FE can render <img src="/uploads/<gym_id>/<file>">
// directly. A no-op when UploadsDir is unset, so the route only appears once
// the binary is configured for it.
func (ctrl *AuthController) RegisterUploadsRoute(r *gin.Engine) {
	if ctrl.UploadsDir == "" {
		return
	}
	r.Static("/uploads", ctrl.UploadsDir)
}

// RegisterLocalPhotoRoute expone GET /api/v1/uploads/local/<entity>/:id
// para que el desktop renderee fotos de socios y productos desde el
// cache local del sidecar (UploadsDir/<entity>/<id>.<ext>). Existe
// específicamente para que el FE NUNCA abra socket directo a R2: el
// sync agent baja las imágenes a disco (upload propio O download tras
// pull) y este handler las sirve. Sólo se monta en el binario sidecar
// — el cloud no tiene cache local.
func (ctrl *AuthController) RegisterLocalPhotoRoute(r *gin.Engine) {
	if ctrl.UploadsDir == "" {
		return
	}
	r.GET("/api/v1/uploads/local/members/:id", ctrl.handleServeLocalMemberPhoto)
	r.GET("/api/v1/uploads/local/products/:id", ctrl.handleServeLocalProductPhoto)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type signupReq struct {
	FullName        string `json:"full_name" validate:"required,min=3,max=100"`
	Email           string `json:"email" validate:"required,email"`
	Phone           string `json:"phone,omitempty"`
	Password        string `json:"password" validate:"required,min=8"`
	PasswordConfirm string `json:"password_confirm" validate:"required"`
}

type signupResp struct {
	UserID         uuid.UUID `json:"user_id"`
	GymID          uuid.UUID `json:"gym_id"`
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	SetupCompleted bool      `json:"setup_completed"`
	SidecarToken   string    `json:"sidecar_token,omitempty"`
	// PIN plaintext del dueño — viaja UNA vez en la respuesta del signup
	// para que el wizard lo muestre en pantalla. El dueño lo memoriza
	// (o lo cambia desde su perfil con POST /auth/me/pin).
	PIN string `json:"pin"`
	// El dueño siempre queda con PIN asignado tras signup. Lo exponemos
	// para que el FE pueda setear has_pin=true en su session local sin
	// tener que pegar /auth/me como sanity check.
	HasPIN bool `json:"has_pin"`
	// PinHash es el bcrypt — útil sólo cuando el signup va por el
	// sidecar (desktop "primer arranque" sin web), para que el mirror
	// local quede con login-pin operativo desde t=0.
	PinHash string `json:"pin_hash,omitempty"`
}

type loginReq struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

type loginResp struct {
	UserID             uuid.UUID  `json:"user_id"`
	GymID              uuid.UUID  `json:"gym_id"`
	FullName           string     `json:"full_name"`
	Email              string     `json:"email"`
	Phone              string     `json:"phone,omitempty"`
	Role               string     `json:"role"`
	GymName            *string    `json:"gym_name"`
	AccessToken        string     `json:"access_token"`
	RefreshToken       string     `json:"refresh_token"`
	SetupCompleted     bool       `json:"setup_completed"`
	TrialEndsAt        *time.Time `json:"trial_ends_at,omitempty"`
	SubscriptionPlan   string     `json:"subscription_plan"`
	MustChangePassword bool       `json:"must_change_password"`
	SidecarToken       string     `json:"sidecar_token,omitempty"`
	HasPIN             bool       `json:"has_pin"`
	// PinHash es el bcrypt del PIN actual del user. Viaja para que el
	// sidecar lo mirroree en su sqlite local — el flujo login-pin offline
	// del kiosko depende de tener el hash localmente. Mismo tier de
	// sensibilidad que password_hash que ya se almacenaba (bcrypt local).
	PinHash string `json:"pin_hash,omitempty"`
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

// createOperatorReq es el wire del POST /users (alta de operador). Modelo
// PIN-first: phone obligatorio (entrega del PIN por WhatsApp), email
// opcional (operadores de barrio rara vez tienen correo), sin password
// (el servidor genera el PIN automáticamente y lo devuelve UNA vez en la
// respuesta).
type createOperatorReq struct {
	FullName string  `json:"full_name" validate:"required,min=3,max=100"`
	Phone    string  `json:"phone" validate:"required"`
	Email    *string `json:"email,omitempty"`
}

// createOperatorResp viaja al FE con el PIN plaintext (mostrado UNA vez +
// como fallback si WhatsApp no estaba conectado). whatsapp_delivery es el
// boolean que el modal usa para pintar el badge verde/ámbar.
type createOperatorResp struct {
	UserID            uuid.UUID `json:"user_id"`
	FullName          string    `json:"full_name"`
	Phone             string    `json:"phone"`
	Email             string    `json:"email"`
	PIN               string    `json:"pin"`
	WhatsAppDelivery  bool      `json:"whatsapp_delivery"`
	WhatsAppSkipped   string    `json:"whatsapp_skipped,omitempty"`
}

type updateOperatorReq struct {
	FullName *string `json:"full_name,omitempty"`
	Email    *string `json:"email,omitempty"`
	Phone    *string `json:"phone,omitempty"`
}

// rotateOperatorPINResp viaja al FE cuando el dueño regenera el PIN del
// operador. Mismo shape que la mitad relevante del 201 de alta.
type rotateOperatorPINResp struct {
	UserID           uuid.UUID `json:"user_id"`
	PIN              string    `json:"pin"`
	WhatsAppDelivery bool      `json:"whatsapp_delivery"`
	WhatsAppSkipped  string    `json:"whatsapp_skipped,omitempty"`
}

type toggleActiveReq struct {
	Active bool `json:"active"`
}

type transferReq struct {
	TargetUserID uuid.UUID `json:"target_user_id" validate:"required"`
	Code         string    `json:"code,omitempty"`
}

type installerTokenResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type deviceResp struct {
	ID          uuid.UUID `json:"id"`
	ClientID    uuid.UUID `json:"client_id"`
	DeviceLabel string    `json:"device_label"`
	CreatedAt   time.Time `json:"created_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

type listDevicesResp struct {
	Items []deviceResp `json:"items"`
	Count int          `json:"count"`
}

type redeemInstallerReq struct {
	Token string `json:"token" validate:"required"`
}

type assignPinReq struct {
	PIN string `json:"pin" validate:"required"`
}

// pinResp is the wire shape the sidecar projects: the desktop reads
// `has_pin` straight off so it can update the grid badge after a
// successful assign without re-fetching /auth/operators.
type pinResp struct {
	HasPIN bool `json:"has_pin"`
}

type redeemInstallerResp struct {
	UserID           uuid.UUID  `json:"user_id"`
	GymID            uuid.UUID  `json:"gym_id"`
	FullName         string     `json:"full_name"`
	Email            string     `json:"email"`
	Phone            string     `json:"phone,omitempty"`
	Role             string     `json:"role"`
	GymName          *string    `json:"gym_name"`
	AccessToken      string     `json:"access_token"`
	RefreshToken     string     `json:"refresh_token"`
	SetupCompleted   bool       `json:"setup_completed"`
	TrialEndsAt      *time.Time `json:"trial_ends_at,omitempty"`
	SubscriptionPlan string     `json:"subscription_plan"`
	SidecarToken     string     `json:"sidecar_token,omitempty"`
	HasPIN           bool       `json:"has_pin"`
	PinHash          string     `json:"pin_hash,omitempty"`
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
		FullName: req.FullName, Email: req.Email, Phone: req.Phone,
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
		SidecarToken:   ctrl.maybeMintSidecarToken(c, out.UserID, out.GymID),
		PIN:            out.PIN,
		HasPIN:         true,
		PinHash:        out.PinHash,
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
		FullName:           out.FullName,
		Email:              out.Email,
		Phone:              out.Phone,
		Role:               out.Role,
		GymName:            out.GymName,
		AccessToken:        out.AccessToken,
		RefreshToken:       out.RefreshToken,
		SetupCompleted:     out.SetupCompleted,
		TrialEndsAt:        out.TrialEndsAt,
		SubscriptionPlan:   out.SubscriptionPlan,
		MustChangePassword: out.MustChangePassword,
		SidecarToken:       ctrl.maybeMintSidecarToken(c, out.UserID, out.GymID),
		HasPIN:             out.HasPIN,
		PinHash:            out.PinHash,
	})
}

// maybeMintSidecarToken inspects X-Cuadra-Client-ID and (when present) calls
// BootstrapSidecarToken to give the sidecar caller its long-lived sk_live_*
// credential in the same response. Idempotent — returns "" when the gym
// already has an active credential for this client_id, telling the sidecar
// to keep its previously stored token.
func (ctrl *AuthController) maybeMintSidecarToken(c *gin.Context, userID, gymID uuid.UUID) string {
	if ctrl.SidecarBootstrap == nil {
		return ""
	}
	clientID := readClientID(c)
	if clientID == uuid.Nil {
		return ""
	}
	tok, err := ctrl.SidecarBootstrap.Execute(c.Request.Context(), usersApp.BootstrapSidecarTokenInput{
		GymID:       gymID,
		UserID:      userID,
		ClientID:    clientID,
		DeviceLabel: c.GetHeader(HeaderDeviceLabel),
	})
	if err != nil {
		// Non-fatal — login already succeeded. The sidecar will re-attempt
		// on its next login if anything went wrong here.
		return ""
	}
	return tok
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

// handleIssueInstaller — POST /api/v1/auth/installer-token (auth required).
// Owner-only call from the dashboard right after web signup. Returns a
// single-use code the operator hands to the desktop installer for a
// zero-password first-launch session.
func (ctrl *AuthController) handleIssueInstaller(c *gin.Context) {
	if ctrl.InstallerBootstrap == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errInstallerBootstrapDisabled)
		return
	}
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errInstallerBootstrapNoAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	out, err := ctrl.InstallerBootstrap.Execute(c.Request.Context(), usersApp.IssueInstallerBootstrapInput{
		GymID: gymID, UserID: userID,
	})
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, installerTokenResp{
		Token:     out.Token,
		ExpiresAt: out.ExpiresAt,
	})
}

// handleRedeemInstaller — POST /api/v1/auth/redeem-installer (no auth).
// The desktop's first launch posts the bootstrap code (alongside its
// X-Cuadra-Client-ID header) to swap it for a full session: operator JWTs
// + sk_live_* sidecar credential. Single-use; subsequent attempts get 410.
func (ctrl *AuthController) handleRedeemInstaller(c *gin.Context) {
	if ctrl.RedeemInstaller == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errInstallerBootstrapDisabled)
		return
	}
	var req redeemInstallerReq
	if !bind(c, &req) {
		return
	}
	clientID := readClientID(c)
	out, err := ctrl.RedeemInstaller.Execute(c.Request.Context(), usersApp.RedeemInstallerBootstrapInput{
		Token:       req.Token,
		ClientID:    clientID,
		DeviceLabel: c.GetHeader(HeaderDeviceLabel),
	})
	if err != nil {
		switch {
		case errors.Is(err, installerbootstrap.ErrNotFound):
			utils.ErrorResponse(c, http.StatusNotFound, err)
		case errors.Is(err, installerbootstrap.ErrAlreadyRedeemed):
			utils.ErrorResponse(c, http.StatusGone, err)
		case errors.Is(err, installerbootstrap.ErrExpired):
			utils.ErrorResponse(c, http.StatusGone, err)
		default:
			utils.ErrorResponse(c, http.StatusInternalServerError, err)
		}
		return
	}
	utils.JsonResponse(c, http.StatusOK, redeemInstallerResp{
		UserID:           out.UserID,
		GymID:            out.GymID,
		FullName:         out.FullName,
		Email:            out.Email,
		Phone:            out.Phone,
		Role:             out.Role,
		GymName:          out.GymName,
		AccessToken:      out.AccessToken,
		RefreshToken:     out.RefreshToken,
		SetupCompleted:   out.SetupCompleted,
		TrialEndsAt:      out.TrialEndsAt,
		SubscriptionPlan: out.SubscriptionPlan,
		SidecarToken:     out.SidecarToken,
		HasPIN:           out.HasPIN,
		PinHash:          out.PinHash,
	})
}

var (
	errInstallerBootstrapDisabled = errors.New("installer bootstrap not configured on this server")
	errInstallerBootstrapNoAuth   = errors.New("autenticación requerida")
	errPINNotConfigured           = errors.New("pin assignment not configured on this server")
	errDevicesNotConfigured       = errors.New("devices listing not configured on this server")
	errR2NotConfigured            = errors.New("R2 storage not configured on this server")
)

// uploadPresignReq — body que el sidecar manda al cloud. `kind` es para
// scoping del key path; `member_id`/`product_id` y `ext` componen el
// nombre final del objeto. `content_type` se bindea al signature del PUT.
// Sólo uno de member_id/product_id se popula según `kind` (mutuamente
// exclusivos, validado abajo).
type uploadPresignReq struct {
	Kind        string `json:"kind" validate:"required"`
	MemberID    string `json:"member_id"`
	ProductID   string `json:"product_id"`
	Ext         string `json:"ext" validate:"required"`
	ContentType string `json:"content_type" validate:"required"`
}

type uploadPresignResp struct {
	UploadURL string    `json:"upload_url"`
	ObjectKey string    `json:"object_key"`
	ExpiresAt time.Time `json:"expires_at"`
}

// uploadPresignGetReq — body para POST /api/v1/uploads/presign-get.
// El sidecar pasa el object_key que tiene guardado en su SQLite local
// (rellenado al momento del upload o al pull desde cloud). El cloud
// verifica que el key cae bajo gyms/<gym_id del token>/... antes de
// firmar — sin esa guarda cualquier sidecar podría leer fotos de
// otro gym.
type uploadPresignGetReq struct {
	ObjectKey string `json:"object_key" validate:"required"`
}

type uploadPresignGetResp struct {
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// handleUploadPresign — POST /api/v1/uploads/presign.
// Mintea una URL presignada para que el sidecar (o el dashboard) suba
// un blob directo a R2. El gym_id sale del middleware (token sidecar
// scoped al gym, o JWT del dueño). Per-kind validamos campos:
//
//   - member_photo:  requiere member_id  + ext (jpg|jpeg|png|webp).
//   - product_photo: requiere product_id + ext (jpg|jpeg|png|webp).
//
// Key naming: gyms/<gym_id>/<subdir>/<entity_id>.<ext>. Determinístico
// para que un re-upload sobreescriba al anterior (no acumulamos
// huérfanos).
func (ctrl *AuthController) handleUploadPresign(c *gin.Context) {
	if ctrl.R2 == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errR2NotConfigured)
		return
	}
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errInstallerBootstrapNoAuth)
		return
	}
	var req uploadPresignReq
	if !bind(c, &req) {
		return
	}
	ext := strings.ToLower(strings.TrimPrefix(req.Ext, "."))
	var subdir, entityID string
	switch req.Kind {
	case "member_photo":
		if req.MemberID == "" {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("member_id required for member_photo"))
			return
		}
		if _, err := uuid.Parse(req.MemberID); err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("member_id must be a uuid"))
			return
		}
		if !isAllowedImageExt(ext) {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("ext must be jpg|jpeg|png|webp"))
			return
		}
		subdir, entityID = "members", req.MemberID
	case "product_photo":
		if req.ProductID == "" {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("product_id required for product_photo"))
			return
		}
		if _, err := uuid.Parse(req.ProductID); err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("product_id must be a uuid"))
			return
		}
		if !isAllowedImageExt(ext) {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("ext must be jpg|jpeg|png|webp"))
			return
		}
		subdir, entityID = "products", req.ProductID
	default:
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("unknown kind"))
		return
	}

	objectKey := fmt.Sprintf("gyms/%s/%s/%s.%s",
		gymID, subdir, entityID, ext,
	)
	ttl := 5 * time.Minute
	uploadURL, err := ctrl.R2.PresignUpload(
		c.Request.Context(), objectKey, req.ContentType, ttl,
	)
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, uploadPresignResp{
		UploadURL: uploadURL,
		ObjectKey: objectKey,
		ExpiresAt: time.Now().UTC().Add(ttl),
	})
}

// handleUploadPresignGet — POST /api/v1/uploads/presign-get.
// Mintea una GET firmada (TTL corto) para que el sidecar baje una foto
// de R2 desde el cache local. El bucket es privado: sin esta firma el
// GET regresaría 403.
//
// Scoping: el object_key DEBE empezar con `gyms/<gym_id del token>/`.
// Sin esa verificación, un sidecar comprometido podría leer fotos de
// CUALQUIER gym pasando un key arbitrario. La firma del token sidecar
// ya está validada por el middleware; aquí amarramos lo que pide al
// scope del gym al que pertenece.
func (ctrl *AuthController) handleUploadPresignGet(c *gin.Context) {
	if ctrl.R2 == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errR2NotConfigured)
		return
	}
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errInstallerBootstrapNoAuth)
		return
	}
	var req uploadPresignGetReq
	if !bind(c, &req) {
		return
	}
	expectedPrefix := fmt.Sprintf("gyms/%s/", gymID)
	if !strings.HasPrefix(req.ObjectKey, expectedPrefix) {
		utils.ErrorResponse(c, http.StatusForbidden, errors.New("object_key fuera del scope del gym"))
		return
	}
	// Defensa contra path traversal — el key debe ser simple (sin ..).
	if strings.Contains(req.ObjectKey, "..") {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("invalid object_key"))
		return
	}
	ttl := 5 * time.Minute
	downloadURL, err := ctrl.R2.PresignDownload(c.Request.Context(), req.ObjectKey, ttl)
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, uploadPresignGetResp{
		DownloadURL: downloadURL,
		ExpiresAt:   time.Now().UTC().Add(ttl),
	})
}

func isAllowedImageExt(ext string) bool {
	switch ext {
	case "jpg", "jpeg", "png", "webp":
		return true
	}
	return false
}

// handleServeLocalMemberPhoto — GET /api/v1/uploads/local/members/:id.
// Sirve el archivo cacheado en UploadsDir/members/<id>.<ext> al FE.
// Delega en serveLocalPhoto para reusar lógica con products.
func (ctrl *AuthController) handleServeLocalMemberPhoto(c *gin.Context) {
	ctrl.serveLocalPhoto(c, "members")
}

// handleServeLocalProductPhoto — GET /api/v1/uploads/local/products/:id.
// Análogo a handleServeLocalMemberPhoto pero sobre UploadsDir/products.
func (ctrl *AuthController) handleServeLocalProductPhoto(c *gin.Context) {
	ctrl.serveLocalPhoto(c, "products")
}

// serveLocalPhoto sirve el archivo cacheado en UploadsDir/<subdir>/<id>.<ext>
// al FE del desktop. Esta es la ÚNICA forma legítima que el FE tiene de
// renderear fotos de socios y productos — nunca abre un <img src> contra
// R2 (bucket privado).
//
// Estrategia: glob para encontrar <id>.* (no sabemos la extensión a
// priori porque el sidecar la deriva del content-type). En el caso raro
// de dos archivos con el mismo id (no debería pasar tras el cleanup en
// writePhotoToDisk), tomamos el primero determinísticamente (orden lex).
//
// 404 cuando:
//   - el id no es un UUID válido,
//   - no existe ningún archivo en cache (la foto aún no fue
//     descargada por el download task, o la entidad no tiene foto).
//
// El FE muestra placeholder en 404, así que un miss aquí no rompe nada
// — el download task la traerá en el próximo tick si existe cloud-side.
func (ctrl *AuthController) serveLocalPhoto(c *gin.Context, subdir string) {
	if ctrl.UploadsDir == "" {
		c.Status(http.StatusNotFound)
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	dir := filepath.Join(ctrl.UploadsDir, subdir)
	matches, err := filepath.Glob(filepath.Join(dir, id+".*"))
	if err != nil || len(matches) == 0 {
		c.Status(http.StatusNotFound)
		return
	}
	// Confinamiento: filepath.Glob no resuelve ".." pero igual rechazamos
	// rutas que se escapen del dir esperado (defensa en profundidad si
	// algún día el id viene de una fuente menos confiable).
	path := matches[0]
	abs, err := filepath.Abs(path)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if !strings.HasPrefix(abs, absDir+string(filepath.Separator)) {
		c.Status(http.StatusNotFound)
		return
	}
	// Cache headers: el archivo es inmutable mientras la entidad no
	// reemplace la foto. Pero como el id no incluye un hash de
	// contenido, una rotación de foto invalidaría el cache. 30s es
	// suficiente para evitar requests redundantes en una vista y lo
	// bastante corto para que un cambio se vea casi inmediatamente.
	c.Header("Cache-Control", "private, max-age=30")
	c.File(path)
}

// handleListDevices — GET /api/v1/auth/devices (owner-only).
// Lista los desktops vinculados al gym del owner autenticado. El modelo
// (sidecar_credentials con idx único partial en (gym_id, client_id)
// WHERE revoked_at IS NULL) ya soporta N pareos por gym; este endpoint
// solamente expone la lista al dashboard para que decida si mostrar el
// CTA de descarga o el guard "ya tienes una, ¿quieres otra?".
func (ctrl *AuthController) handleListDevices(c *gin.Context) {
	if ctrl.SidecarTokens == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errDevicesNotConfigured)
		return
	}
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errInstallerBootstrapNoAuth)
		return
	}
	creds, err := ctrl.SidecarTokens.ListActiveByGym(c.Request.Context(), gymID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	items := make([]deviceResp, len(creds))
	for i, cr := range creds {
		items[i] = deviceResp{
			ID:          cr.ID,
			ClientID:    cr.ClientID,
			DeviceLabel: cr.DeviceLabel,
			CreatedAt:   cr.CreatedAt,
			LastSeenAt:  cr.LastSeenAt,
		}
	}
	utils.JsonResponse(c, http.StatusOK, listDevicesResp{
		Items: items,
		Count: len(items),
	})
}

// handleAssignSelfPIN — POST /api/v1/auth/me/pin (authed).
// Sets or replaces the authenticated user's reception PIN. The PIN comes in
// plaintext; the use case hashes with bcrypt. Returns the post-state has_pin
// flag so the desktop can reflect it without re-querying.
func (ctrl *AuthController) handleAssignSelfPIN(c *gin.Context) {
	if ctrl.AssignSelfPIN == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errPINNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	var req assignPinReq
	if !bind(c, &req) {
		return
	}
	if _, err := ctrl.AssignSelfPIN.Execute(c.Request.Context(), usersApp.AssignSelfPINInput{
		GymID:  gymID,
		UserID: userID,
		PIN:    req.PIN,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, pinResp{HasPIN: true})
}

// handleClearSelfPIN — DELETE /api/v1/auth/me/pin (authed). Idempotent.
func (ctrl *AuthController) handleClearSelfPIN(c *gin.Context) {
	if ctrl.ClearSelfPIN == nil {
		utils.ErrorResponse(c, http.StatusNotImplemented, errPINNotConfigured)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	if err := ctrl.ClearSelfPIN.Execute(c.Request.Context(), usersApp.ClearSelfPINInput{
		GymID: gymID, UserID: userID,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, pinResp{HasPIN: false})
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

// handleListOperators — GET /api/v1/users (owner-only). Lista a todo el staff
// del gym del owner autenticado (dueño + operadores). El query param
// `include_inactive=true|false` filtra por `active`. Default: true (la página
// Operadores muestra desactivados con badge gris).
//
// Wire shape: `{ items: operatorWire[] }` — espeja la interfaz Operator del
// desktop (useOperators). El handler delega a `Users.ListByGym` ya existente
// y filtra in-process; el volumen es bajo (<= 10 por gym, MaxOperatorsPerGym).
func (ctrl *AuthController) handleListOperators(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	if ctrl.Users == nil || ctrl.UoW == nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, errBadAuth)
		return
	}
	includeInactive := c.DefaultQuery("include_inactive", "true") == "true"
	tx, err := ctrl.UoW.Query(c.Request.Context())
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	rows, err := ctrl.Users.ListByGym(tx, gymID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]operatorWire, 0, len(rows))
	for _, u := range rows {
		if !includeInactive && !u.Active {
			continue
		}
		items = append(items, toOperatorWire(u))
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": items})
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
		FullName: req.FullName,
		Phone:    req.Phone,
		Email:    req.Email,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, createOperatorResp{
		UserID:           out.UserID,
		FullName:         out.FullName,
		Phone:            out.Phone,
		Email:            out.Email,
		PIN:              out.PIN,
		WhatsAppDelivery: out.WhatsAppDelivery,
		WhatsAppSkipped:  out.WhatsAppSkipped,
	})
}

func (ctrl *AuthController) handleRotateOperatorPIN(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	targetID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.RotateOperatorPIN.Execute(c.Request.Context(), usersApp.RotateOperatorPINInput{
		GymID: gymID, ActorUserID: actorID, TargetID: targetID,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, rotateOperatorPINResp{
		UserID:           out.UserID,
		PIN:              out.PIN,
		WhatsAppDelivery: out.WhatsAppDelivery,
		WhatsAppSkipped:  out.WhatsAppSkipped,
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
