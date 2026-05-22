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
	cs, _ := json.Marshal(orEmptyMap(g.ChargeSettings))
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
		StripeCustomerID:         g.StripeCustomerID,
		SetupCompletedAt:         g.SetupCompletedAt,
		WhatsAppBusinessPhone:    g.WhatsAppBusinessPhone,
		WhatsAppBusinessTokenEnc: g.WhatsAppBusinessTokenEnc,
		WhatsAppConnectedAt:      g.WhatsAppConnectedAt,
		KioskSettings:            string(ks),
		ChargeSettings:           string(cs),
	}
}

// orEmptyMap garantiza que un map nil se serialice como "{}" en lugar
// de "null". JSONB/TEXT con CHECK(json_valid(...)) en SQLite rechaza
// null literal en algunas migrations.
func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
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
	var charge map[string]any
	if m.ChargeSettings != "" {
		_ = json.Unmarshal([]byte(m.ChargeSettings), &charge)
	}
	if charge == nil {
		charge = map[string]any{}
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
		StripeCustomerID:         m.StripeCustomerID,
		SetupCompletedAt:         m.SetupCompletedAt,
		WhatsAppBusinessPhone:    m.WhatsAppBusinessPhone,
		WhatsAppBusinessTokenEnc: m.WhatsAppBusinessTokenEnc,
		WhatsAppConnectedAt:      m.WhatsAppConnectedAt,
		KioskSettings:            kiosk,
		ChargeSettings:           charge,
		CreatedAt:                m.CreatedAt,
		UpdatedAt:                m.UpdatedAt,
		DeletedAt:                m.DeletedAt,
	}
}
