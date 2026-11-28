// me_controller.go wires the bootstrap endpoints both frontends consume on
// load — GET /api/v1/auth/me and GET /api/v1/gyms/me — and the FE-driven
// PATCH /api/v1/gyms/me shape (full profile, not the wizard step-2 path).
//
// These live next to the auth controller because they share the same auth
// surface (operator JWT) and the same dependencies (UserRepository,
// GymRepository, UoW). Keeping them in their own file makes the diff against
// the cloud projector ADR-008 changes obvious.
package controllers

import (
	"errors"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	gymApp "github.com/cuadra/cuadra-core/src/modules/gyms/app"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	userApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// Límites de la subida del logo. 5 MB alcanza para una foto del logo
// tomada con el celular; lo que se publica sale reducido (PrepareUpload).
// El FE (GymLogoField) valida el mismo tope antes de mandar.
const (
	maxLogoBytes = 5 * 1024 * 1024 // 5 MiB
	mimeSniffLen = 512             // http.DetectContentType reads up to 512 B
)

// logoMimes — formatos que acepta la subida (sniff del contenido real).
var logoMimes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
	"image/gif":  {},
}

var (
	errLogoUploadUnavailable = errors.New("subir el logo no está disponible por ahora")
	errLogoMissingFile       = errors.New("elige una imagen para el logo")
	errLogoTooLarge          = errors.New("la imagen pesa más de 5 MB; usa una más ligera")
	errLogoBadMime           = errors.New("formato no soportado: usa PNG, JPG o WebP")
	errLogoNotOwner          = errors.New("sólo el dueño puede cambiar el logo")
)

// ---------------------------------------------------------------------------
// Wire shapes — match cuadra-desktop/cuadra-dashboard hooks exactly.
// ---------------------------------------------------------------------------

type meUserWire struct {
	UserID          uuid.UUID  `json:"user_id"`
	FullName        string     `json:"full_name"`
	Email           string     `json:"email"`
	Phone           *string    `json:"phone"`
	Role            string     `json:"role"`
	HasPIN          bool       `json:"has_pin"`
	EmailVerifiedAt *time.Time `json:"email_verified_at"`
	// Permissions: lo que el usuario puede hacer (todos para el dueño).
	Permissions []string `json:"permissions"`
}

type meGymWire struct {
	GymID              uuid.UUID  `json:"gym_id"`
	Name               string     `json:"name"`
	SetupCompleted     bool       `json:"setup_completed"`
	TrialEndsAt        *time.Time `json:"trial_ends_at,omitempty"`
	AllFeaturesAccess  bool       `json:"all_features_access"`
	SubscriptionPlan   string     `json:"subscription_plan"`
	SubscriptionStatus string     `json:"subscription_status"`
	SubscriptionEndsAt *time.Time `json:"subscription_ends_at,omitempty"`
}

type meResponse struct {
	User meUserWire `json:"user"`
	Gym  meGymWire  `json:"gym"`
}

// updateMeReq is the PATCH /api/v1/auth/me payload — the current user
// editing their own profile. Email + role + active live elsewhere; this
// is intentionally the smallest editable surface a non-owner needs.
type updateMeReq struct {
	FullName *string `json:"full_name,omitempty"`
	Phone    *string `json:"phone,omitempty"`
}

// gymProfileWire mirrors cuadra-desktop's `GymProfile` interface. The naming
// differs from the domain field names (legal_name vs RazonSocial, etc.) — we
// rename at the wire edge per CLAUDE.md's frontend-driven convention.
type gymProfileWire struct {
	ID                 uuid.UUID `json:"id"`
	Name               string    `json:"name"`
	City               string    `json:"city"`
	WhatsAppNumber     *string   `json:"whatsapp_number"`
	Timezone           string    `json:"timezone"`
	RFC                *string   `json:"rfc"`
	LegalName          *string   `json:"legal_name"`
	PostalCode         *string   `json:"postal_code"`
	TaxRegime          *string   `json:"tax_regime"`
	LogoURL            *string   `json:"logo_url"`
	PrimaryColor       *string   `json:"primary_color"`
	SecondaryColor     *string   `json:"secondary_color"`
	OpenTime           *string   `json:"open_time"`
	CloseTime          *string   `json:"close_time"`
	KioskVolume        int       `json:"kiosk_volume"`
	KioskFeedbackTTLMs int       `json:"kiosk_feedback_ttl_ms"`
	AccessWebhookURL   *string   `json:"access_webhook_url,omitempty"`
	AccessWebhookSet   bool      `json:"access_webhook_secret_set"`
	SetupCompleted     bool      `json:"setup_completed"`
}

// updateGymProfileWireReq mirrors FE UpdateGymProfileInput. Every field is a
// pointer so callers can patch a subset; the controller maps to the domain
// ProfileUpdate (which uses the BE-internal naming).
type updateGymProfileWireReq struct {
	Name                *string `json:"name,omitempty"`
	City                *string `json:"city,omitempty"`
	WhatsAppNumber      *string `json:"whatsapp_number,omitempty"`
	Timezone            *string `json:"timezone,omitempty"`
	RFC                 *string `json:"rfc,omitempty"`
	LegalName           *string `json:"legal_name,omitempty"`
	PostalCode          *string `json:"postal_code,omitempty"`
	TaxRegime           *string `json:"tax_regime,omitempty"`
	LogoURL             *string `json:"logo_url,omitempty"`
	PrimaryColor        *string `json:"primary_color,omitempty"`
	SecondaryColor      *string `json:"secondary_color,omitempty"`
	OpenTime            *string `json:"open_time,omitempty"`
	CloseTime           *string `json:"close_time,omitempty"`
	KioskVolume         *int    `json:"kiosk_volume,omitempty"`
	KioskFeedbackTTLMs  *int    `json:"kiosk_feedback_ttl_ms,omitempty"`
	AccessWebhookURL    *string `json:"access_webhook_url,omitempty"`
	AccessWebhookSecret *string `json:"access_webhook_secret,omitempty"`
}

// setupStatusWire mirrors cuadra-desktop's SetupStatus from useSetupStatus.
type setupStatusWire struct {
	CurrentStep          int             `json:"current_step"`
	SetupCompleted       bool            `json:"setup_completed"`
	GymName              *string         `json:"gym_name"`
	City                 *string         `json:"city"`
	WhatsApp             *string         `json:"whatsapp"`
	MembershipTypesCount int             `json:"membership_types_count"`
	PaymentMethods       map[string]bool `json:"payment_methods"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *AuthController) handleGetMe(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok || userID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	if ctrl.Users == nil || ctrl.Gyms == nil || ctrl.UoW == nil {
		// Defensive: the route only mounts once these are wired in main.go.
		// Returning 500 surfaces the misconfiguration loudly during dev.
		utils.ErrorResponse(c, http.StatusInternalServerError, errBadAuth)
		return
	}
	tx, err := ctrl.UoW.Query(c.Request.Context())
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	user, err := ctrl.Users.GetByID(tx, userID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	gym, err := ctrl.Gyms.GetByID(tx, gymID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}

	gymName := ""
	if gym.Name != nil {
		gymName = *gym.Name
	}
	utils.JsonResponse(c, http.StatusOK, meResponse{
		User: meUserWire{
			UserID:          user.ID,
			FullName:        user.FullName,
			Email:           user.Email,
			Phone:           user.Phone,
			Role:            user.Role,
			HasPIN:          user.HasPIN(),
			EmailVerifiedAt: user.EmailVerifiedAt,
			Permissions:     permissionKeys(user.EffectivePermissions()),
		},
		Gym: meGymWire{
			GymID:              gym.ID,
			Name:               gymName,
			SetupCompleted:     gym.IsSetupComplete(),
			TrialEndsAt:        gym.TrialEndsAt,
			AllFeaturesAccess:  gym.AllFeaturesAccess,
			SubscriptionPlan:   gym.SubscriptionPlan,
			SubscriptionStatus: gym.SubscriptionStatus,
			SubscriptionEndsAt: gym.SubscriptionEndsAt,
		},
	})
}

// handleUpdateMe lets the authenticated user edit their own profile
// (full_name, phone). Reuses the UpdateOperator use case with target ==
// actor; the gym scope check inside UC-007 keeps cross-tenant edits out.
// Available to any role — operators don't need owner approval to fix
// their own display name.
func (ctrl *AuthController) handleUpdateMe(c *gin.Context) {
	userID, ok := middleware.GetUserID(c)
	if !ok || userID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	if ctrl.UpdateOperator == nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, errBadAuth)
		return
	}
	var req updateMeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	updated, err := ctrl.UpdateOperator.Execute(c.Request.Context(), userApp.UpdateOperatorInput{
		GymID:       gymID,
		ActorUserID: userID,
		TargetID:    userID,
		FullName:    req.FullName,
		Phone:       req.Phone,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, meUserWire{
		UserID:      updated.ID,
		FullName:    updated.FullName,
		Email:       updated.Email,
		Phone:       updated.Phone,
		Role:        updated.Role,
		HasPIN:      updated.HasPIN(),
		Permissions: permissionKeys(updated.EffectivePermissions()),
	})
}

func (ctrl *AuthController) handleGetGymProfile(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	if ctrl.Gyms == nil || ctrl.UoW == nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, errBadAuth)
		return
	}
	tx, err := ctrl.UoW.Query(c.Request.Context())
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	gym, err := ctrl.Gyms.GetByID(tx, gymID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toGymProfileWire(gym))
}

// handleUpdateProfileWire is the FE-driven PATCH /api/v1/gyms/me. Replaces the
// older handleUpdateProfile in route registration order; the wire shape uses
// FE field names (legal_name, postal_code, tax_regime, whatsapp_number).
func (ctrl *AuthController) handleUpdateProfileWire(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	var req updateGymProfileWireReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	upd := gymDomain.ProfileUpdate{
		Name:                req.Name,
		City:                req.City,
		WhatsApp:            req.WhatsAppNumber,
		Timezone:            req.Timezone,
		RFC:                 req.RFC,
		RazonSocial:         req.LegalName,
		CodigoPostal:        req.PostalCode,
		RegimenFiscal:       req.TaxRegime,
		LogoURL:             req.LogoURL,
		PrimaryColor:        req.PrimaryColor,
		SecondaryColor:      req.SecondaryColor,
		OpenTime:            req.OpenTime,
		CloseTime:           req.CloseTime,
		KioskVolume:         req.KioskVolume,
		KioskFeedbackTTLMs:  req.KioskFeedbackTTLMs,
		AccessWebhookURL:    req.AccessWebhookURL,
		AccessWebhookSecret: req.AccessWebhookSecret,
	}
	// Si el dueño tocó campos con función propia en el catálogo (branding,
	// datos fiscales, webhook de acceso), validar cada una contra el plan.
	// El resto del PATCH (city, horario, kiosko) no llega al gate.
	if gated := upd.GatedFeatures(); len(gated) > 0 {
		if !middleware.EnforceFeaturesInline(c, ctrl.Gyms, ctrl.UoW, gated...) {
			return
		}
	}
	out, err := ctrl.UpdateProfile.Execute(c.Request.Context(), gymApp.UpdateProfileInput{
		GymID: gymID, ActorUserID: userID,
		Update: upd,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toGymProfileWire(out))
}

// chargeSettingsWire es la representación FE de gym.charge_settings. Todos
// los campos son opcionales en el response porque un gym recién creado tiene
// `charge_settings = {}` — la ausencia significa "no configurado todavía".
type chargeSettingsWire struct {
	ChargesEnrollment    *bool    `json:"charges_enrollment,omitempty"`
	ChargesMaintenance   *bool    `json:"charges_maintenance,omitempty"`
	EnrollmentAmount     *float64 `json:"enrollment_amount,omitempty"`
	MaintenanceAmount    *float64 `json:"maintenance_amount,omitempty"`
	MaintenanceFrequency *string  `json:"maintenance_frequency,omitempty"`
}

// updateChargeSettingsReq es el PATCH /gyms/me/charge-settings. Cada
// campo nil = "no tocar". El FE manda sólo lo que cambia (parche), no
// el set completo.
type updateChargeSettingsReq struct {
	ChargesEnrollment    *bool    `json:"charges_enrollment,omitempty"`
	ChargesMaintenance   *bool    `json:"charges_maintenance,omitempty"`
	EnrollmentAmount     *float64 `json:"enrollment_amount,omitempty"`
	MaintenanceAmount    *float64 `json:"maintenance_amount,omitempty"`
	MaintenanceFrequency *string  `json:"maintenance_frequency,omitempty"`
}

func toChargeSettingsWire(m map[string]any) chargeSettingsWire {
	out := chargeSettingsWire{}
	if v, ok := m["charges_enrollment"].(bool); ok {
		out.ChargesEnrollment = &v
	}
	if v, ok := m["charges_maintenance"].(bool); ok {
		out.ChargesMaintenance = &v
	}
	if v, ok := m["enrollment_amount"].(float64); ok {
		out.EnrollmentAmount = &v
	}
	if v, ok := m["maintenance_amount"].(float64); ok {
		out.MaintenanceAmount = &v
	}
	if v, ok := m["maintenance_frequency"].(string); ok && v != "" {
		out.MaintenanceFrequency = &v
	}
	return out
}

func (ctrl *AuthController) handleGetChargeSettings(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	if ctrl.Gyms == nil || ctrl.UoW == nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, errBadAuth)
		return
	}
	tx, err := ctrl.UoW.Query(c.Request.Context())
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	gym, err := ctrl.Gyms.GetByID(tx, gymID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toChargeSettingsWire(gym.ChargeSettings))
}

func (ctrl *AuthController) handleUpdateChargeSettings(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	if ctrl.UpdateChargeSet == nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, errBadAuth)
		return
	}
	var req updateChargeSettingsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	gym, err := ctrl.UpdateChargeSet.Execute(c.Request.Context(), gymApp.UpdateChargeSettingsInput{
		GymID:                gymID,
		ActorUserID:          userID,
		ChargesEnrollment:    req.ChargesEnrollment,
		ChargesMaintenance:   req.ChargesMaintenance,
		EnrollmentAmount:     req.EnrollmentAmount,
		MaintenanceAmount:    req.MaintenanceAmount,
		MaintenanceFrequency: req.MaintenanceFrequency,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toChargeSettingsWire(gym.ChargeSettings))
}

func (ctrl *AuthController) handleSetupStatus(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	if ctrl.Gyms == nil || ctrl.UoW == nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, errBadAuth)
		return
	}
	tx, err := ctrl.UoW.Query(c.Request.Context())
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	gym, err := ctrl.Gyms.GetByID(tx, gymID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	mtCount := 0
	if ctrl.MembershipTypes != nil {
		mtCount, _ = ctrl.MembershipTypes.CountByGym(tx, gymID)
	}
	methods := map[string]bool{"cash": false, "transfer": false, "card": false}
	for _, m := range gym.PaymentMethods {
		methods[m] = true
	}
	resp := setupStatusWire{
		CurrentStep:          gym.NextSetupStep(mtCount > 0),
		SetupCompleted:       gym.IsSetupComplete(),
		GymName:              gym.Name,
		City:                 gym.City,
		WhatsApp:             gym.WhatsApp,
		MembershipTypesCount: mtCount,
		PaymentMethods:       methods,
	}
	utils.JsonResponse(c, http.StatusOK, resp)
}

// handleUploadLogo — POST /api/v1/gyms/me/logo (multipart, campo "file").
// El dueño sube su logo desde un botón; SetGymLogo lo deja como PNG limpio
// en el bucket público y guarda la URL absoluta en el gym. Llega del
// dashboard (JWT) o del desktop vía GymLogoSidecarProxy (sk_live_*).
//
// El logo es parte de la feature branding (gym.GatedFeatures), así que la
// subida pasa por el mismo gate inline que PATCH /gyms/me con logo_url.
func (ctrl *AuthController) handleUploadLogo(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	if !ctrl.SetLogo.CanUpload() {
		utils.ErrorResponse(c, http.StatusServiceUnavailable, errLogoUploadUnavailable)
		return
	}
	if !middleware.EnforceFeaturesInline(c, ctrl.Gyms, ctrl.UoW, gymDomain.FeatureBranding) {
		return
	}

	fh, err := c.FormFile("file")
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errLogoMissingFile)
		return
	}
	if fh.Size > maxLogoBytes {
		utils.ErrorResponse(c, http.StatusBadRequest, errLogoTooLarge)
		return
	}
	src, err := fh.Open()
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	defer src.Close()
	raw, err := io.ReadAll(io.LimitReader(src, maxLogoBytes+1))
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errLogoMissingFile)
		return
	}
	if len(raw) > maxLogoBytes {
		utils.ErrorResponse(c, http.StatusBadRequest, errLogoTooLarge)
		return
	}
	// Sniff del contenido real, no del header del multipart: el navegador
	// reporta image/png para un .exe renombrado. PrepareUpload vuelve a
	// validar al decodificar; esto corta antes lo obvio.
	if _, ok := logoMimes[http.DetectContentType(raw[:min(len(raw), mimeSniffLen)])]; !ok {
		utils.ErrorResponse(c, http.StatusBadRequest, errLogoBadMime)
		return
	}

	g, err := ctrl.SetLogo.Upload(c.Request.Context(), gymApp.UploadGymLogoInput{
		GymID: gymID, ActorUserID: userID, Image: raw,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"logo_url": g.LogoURL})
}

// handleRemoveLogo — DELETE /api/v1/gyms/me/logo. Sin gate de plan: un gym
// que dejó Plus debe poder quitar su logo.
func (ctrl *AuthController) handleRemoveLogo(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	if ctrl.SetLogo == nil {
		utils.ErrorResponse(c, http.StatusServiceUnavailable, errLogoUploadUnavailable)
		return
	}
	if _, err := ctrl.SetLogo.Remove(c.Request.Context(), gymID, userID); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"logo_url": nil})
}

// requireOwnerOrSidecarToken — con JWT exige role=owner; con sk_live_* deja
// pasar: la credencial de máquina la emitió el pareo del dueño y el proxy
// del sidecar ya validó role=owner contra su JWT local antes de reenviar.
// Mismo contrato que el ceremony de WhatsApp (notifications).
func requireOwnerOrSidecarToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		if middleware.GetAuthMethod(c) == "sidecar_token" {
			c.Next()
			return
		}
		if role, _ := middleware.GetRole(c); role != "owner" {
			utils.ErrorResponse(c, http.StatusForbidden, errLogoNotOwner)
			c.Abort()
			return
		}
		c.Next()
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toGymProfileWire(g *gymDomain.Gym) gymProfileWire {
	if g == nil {
		return gymProfileWire{}
	}
	out := gymProfileWire{
		ID:             g.ID,
		Timezone:       g.Timezone,
		WhatsAppNumber: g.WhatsApp,
		RFC:            g.RFC,
		LegalName:      g.RazonSocial,
		PostalCode:     g.CodigoPostal,
		TaxRegime:      g.RegimenFiscal,
		LogoURL:        g.LogoURL,
		PrimaryColor:   g.PrimaryColor,
		SecondaryColor: g.SecondaryColor,
		OpenTime:       g.OpenTime,
		CloseTime:      g.CloseTime,
		SetupCompleted: g.IsSetupComplete(),
		// KioskSettings defaults (audio_volume / feedback_ttl_ms) match the
		// NewTrialGym seed in gym.go.
		KioskVolume:        readKioskInt(g.KioskSettings, "audio_volume", 80),
		KioskFeedbackTTLMs: readKioskInt(g.KioskSettings, "feedback_ttl_ms", 4000),
		AccessWebhookURL:   readKioskStrPtr(g.KioskSettings, "access_webhook_url"),
		AccessWebhookSet:   readKioskStr(g.KioskSettings, "access_webhook_secret") != "",
	}
	if g.Name != nil {
		out.Name = *g.Name
	}
	if g.City != nil {
		out.City = *g.City
	}
	return out
}

// operatorWire mirrors cuadra-desktop's `Operator` interface. `has_pin`
// se expone para que la pantalla de Operadores pueda pintar el badge
// "PIN" sin re-pegar otro endpoint, y para que el modal de edición
// decida si esconder la acción legacy "Resetear contraseña" (operadores
// PIN-first sin email no la necesitan).
type operatorWire struct {
	ID                 uuid.UUID  `json:"id"`
	GymID              uuid.UUID  `json:"gym_id"`
	Email              string     `json:"email"`
	FullName           string     `json:"full_name"`
	Phone              *string    `json:"phone"`
	Role               string     `json:"role"`
	Active             bool       `json:"active"`
	MustChangePassword bool       `json:"must_change_password"`
	HasPIN             bool       `json:"has_pin"`
	LastLoginAt        *time.Time `json:"last_login_at"`
	CreatedAt          time.Time  `json:"created_at"`
	Permissions        []string   `json:"permissions"`
}

func toOperatorWire(u *userDomain.User) operatorWire {
	if u == nil {
		return operatorWire{}
	}
	return operatorWire{
		ID:                 u.ID,
		GymID:              u.GymID,
		Email:              u.Email,
		FullName:           u.FullName,
		Phone:              u.Phone,
		Role:               u.Role,
		Active:             u.Active,
		MustChangePassword: u.MustChangePassword,
		HasPIN:             u.HasPIN(),
		LastLoginAt:        u.LastLoginAt,
		CreatedAt:          u.CreatedAt,
		Permissions:        permissionKeys(u.EffectivePermissions()),
	}
}

func readKioskInt(m map[string]any, key string, fallback int) int {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return fallback
}

func readKioskStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func readKioskStrPtr(m map[string]any, key string) *string {
	v := readKioskStr(m, key)
	if v == "" {
		return nil
	}
	return &v
}
