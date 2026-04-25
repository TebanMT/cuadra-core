//go:build server

package repositories

import (
	"encoding/json"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	"github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/models"
)

// toModel and toDomain are the canonical translators between the GORM model
// and the domain entity. JSON columns (payment_methods, kiosk_settings) round
// trip via Go-side marshalling — no PG-specific types leak into the domain.

func toModel(g *gymDomain.Gym) models.GymModel {
	pm, _ := json.Marshal(g.PaymentMethods)
	ks, _ := json.Marshal(g.KioskSettings)
	return models.GymModel{
		ID:                       g.ID,
		GymID:                    g.ID,
		Version:                  g.Version,
		CreatedAt:                g.CreatedAt,
		UpdatedAt:                g.UpdatedAt,
		DeletedAt:                g.DeletedAt,
		Name:                     g.Name,
		City:                     g.City,
		WhatsApp:                 g.WhatsApp,
		Country:                  g.Country,
		Timezone:                 g.Timezone,
		RFC:                      g.RFC,
		RazonSocial:              g.RazonSocial,
		CodigoPostal:             g.CodigoPostal,
		RegimenFiscal:            g.RegimenFiscal,
		LogoURL:                  g.LogoURL,
		PrimaryColor:             g.PrimaryColor,
		SecondaryColor:           g.SecondaryColor,
		PaymentMethods:           string(pm),
		OpenTime:                 g.OpenTime,
		CloseTime:                g.CloseTime,
		SubscriptionPlan:         g.SubscriptionPlan,
		TrialEndsAt:              g.TrialEndsAt,
		SubscriptionEndsAt:       g.SubscriptionEndsAt,
		SubscriptionStatus:       g.SubscriptionStatus,
		SetupCompletedAt:         g.SetupCompletedAt,
		WhatsAppBusinessPhone:    g.WhatsAppBusinessPhone,
		WhatsAppBusinessTokenEnc: g.WhatsAppBusinessTokenEnc,
		WhatsAppConnectedAt:      g.WhatsAppConnectedAt,
		KioskSettings:            string(ks),
	}
}

func toDomain(m *models.GymModel) *gymDomain.Gym {
	var paymentMethods []string
	if m.PaymentMethods != "" {
		_ = json.Unmarshal([]byte(m.PaymentMethods), &paymentMethods)
	}
	if paymentMethods == nil {
		paymentMethods = []string{}
	}
	var kiosk map[string]any
	if m.KioskSettings != "" {
		_ = json.Unmarshal([]byte(m.KioskSettings), &kiosk)
	}
	return &gymDomain.Gym{
		ID:                       m.ID,
		Version:                  m.Version,
		Name:                     m.Name,
		City:                     m.City,
		WhatsApp:                 m.WhatsApp,
		Country:                  m.Country,
		Timezone:                 m.Timezone,
		RFC:                      m.RFC,
		RazonSocial:              m.RazonSocial,
		CodigoPostal:             m.CodigoPostal,
		RegimenFiscal:            m.RegimenFiscal,
		LogoURL:                  m.LogoURL,
		PrimaryColor:             m.PrimaryColor,
		SecondaryColor:           m.SecondaryColor,
		PaymentMethods:           paymentMethods,
		OpenTime:                 m.OpenTime,
		CloseTime:                m.CloseTime,
		SubscriptionPlan:         m.SubscriptionPlan,
		TrialEndsAt:              m.TrialEndsAt,
		SubscriptionEndsAt:       m.SubscriptionEndsAt,
		SubscriptionStatus:       m.SubscriptionStatus,
		SetupCompletedAt:         m.SetupCompletedAt,
		WhatsAppBusinessPhone:    m.WhatsAppBusinessPhone,
		WhatsAppBusinessTokenEnc: m.WhatsAppBusinessTokenEnc,
		WhatsAppConnectedAt:      m.WhatsAppConnectedAt,
		KioskSettings:            kiosk,
		CreatedAt:                m.CreatedAt,
		UpdatedAt:                m.UpdatedAt,
		DeletedAt:                m.DeletedAt,
	}
}
