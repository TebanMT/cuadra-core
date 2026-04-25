package controllers

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var errBadAuth = errors.New("token de autenticación faltante o inválido")
var errBadID = errors.New("id inválido")

// bind parses + validates the JSON body. Returns true on success; on failure
// it has already written a 400 response and the caller should `return`.
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
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return uuid.Nil, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// Wire response shapes
// ---------------------------------------------------------------------------

type gymResponse struct {
	ID                 uuid.UUID  `json:"id"`
	Name               *string    `json:"name"`
	City               *string    `json:"city"`
	WhatsApp           *string    `json:"whatsapp"`
	Country            string     `json:"country"`
	Timezone           string     `json:"timezone"`
	RFC                *string    `json:"rfc"`
	RazonSocial        *string    `json:"razon_social"`
	CodigoPostal       *string    `json:"codigo_postal"`
	RegimenFiscal      *string    `json:"regimen_fiscal"`
	LogoURL            *string    `json:"logo_url"`
	PrimaryColor       *string    `json:"primary_color"`
	SecondaryColor     *string    `json:"secondary_color"`
	PaymentMethods     []string   `json:"payment_methods"`
	OpenTime           *string    `json:"open_time"`
	CloseTime          *string    `json:"close_time"`
	SubscriptionPlan   string     `json:"subscription_plan"`
	SubscriptionStatus string     `json:"subscription_status"`
	TrialEndsAt        *time.Time `json:"trial_ends_at,omitempty"`
	SetupCompletedAt   *time.Time `json:"setup_completed_at,omitempty"`
	NextSetupStep      int        `json:"next_setup_step"`
}

func toGymResponse(g *gymDomain.Gym) gymResponse {
	if g == nil {
		return gymResponse{}
	}
	return gymResponse{
		ID:                 g.ID,
		Name:               g.Name,
		City:               g.City,
		WhatsApp:           g.WhatsApp,
		Country:            g.Country,
		Timezone:           g.Timezone,
		RFC:                g.RFC,
		RazonSocial:        g.RazonSocial,
		CodigoPostal:       g.CodigoPostal,
		RegimenFiscal:      g.RegimenFiscal,
		LogoURL:            g.LogoURL,
		PrimaryColor:       g.PrimaryColor,
		SecondaryColor:     g.SecondaryColor,
		PaymentMethods:     g.PaymentMethods,
		OpenTime:           g.OpenTime,
		CloseTime:          g.CloseTime,
		SubscriptionPlan:   g.SubscriptionPlan,
		SubscriptionStatus: g.SubscriptionStatus,
		TrialEndsAt:        g.TrialEndsAt,
		SetupCompletedAt:   g.SetupCompletedAt,
		// NextSetupStep without "has membership type" knowledge is approximate;
		// the wizard caller queries that separately. We pass 0 when complete.
		NextSetupStep: g.NextSetupStep(false),
	}
}

type userResponse struct {
	ID                 uuid.UUID `json:"id"`
	GymID              uuid.UUID `json:"gym_id"`
	Email              string    `json:"email"`
	FullName           string    `json:"full_name"`
	Phone              *string   `json:"phone,omitempty"`
	Role               string    `json:"role"`
	Active             bool      `json:"active"`
	MustChangePassword bool      `json:"must_change_password"`
}

func toUserResponse(u *userDomain.User) userResponse {
	if u == nil {
		return userResponse{}
	}
	return userResponse{
		ID:                 u.ID,
		GymID:              u.GymID,
		Email:              u.Email,
		FullName:           u.FullName,
		Phone:              u.Phone,
		Role:               u.Role,
		Active:             u.Active,
		MustChangePassword: u.MustChangePassword,
	}
}
