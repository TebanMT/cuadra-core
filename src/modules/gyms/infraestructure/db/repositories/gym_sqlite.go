//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	gymErrors "github.com/cuadra/cuadra-core/src/modules/gyms/domain/errors"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type GymSQLiteRepository struct{}

func NewGymSQLiteRepository() *GymSQLiteRepository { return &GymSQLiteRepository{} }

type sqliteGymRow struct {
	ID                       string         `db:"id"`
	GymID                    string         `db:"gym_id"`
	Version                  int            `db:"version"`
	CreatedAt                int64          `db:"created_at"`
	UpdatedAt                int64          `db:"updated_at"`
	DeletedAt                sql.NullInt64  `db:"deleted_at"`
	SyncedAt                 sql.NullInt64  `db:"synced_at"`
	Name                     sql.NullString `db:"name"`
	City                     sql.NullString `db:"city"`
	WhatsApp                 sql.NullString `db:"whatsapp"`
	Country                  string         `db:"country"`
	Timezone                 string         `db:"timezone"`
	RFC                      sql.NullString `db:"rfc"`
	RazonSocial              sql.NullString `db:"razon_social"`
	CodigoPostal             sql.NullString `db:"codigo_postal"`
	RegimenFiscal            sql.NullString `db:"regimen_fiscal"`
	LogoURL                  sql.NullString `db:"logo_url"`
	PrimaryColor             sql.NullString `db:"primary_color"`
	SecondaryColor           sql.NullString `db:"secondary_color"`
	PaymentMethods           string         `db:"payment_methods"`
	OpenTime                 sql.NullString `db:"open_time"`
	CloseTime                sql.NullString `db:"close_time"`
	SubscriptionPlan         string         `db:"subscription_plan"`
	TrialEndsAt              sql.NullInt64  `db:"trial_ends_at"`
	SubscriptionEndsAt       sql.NullInt64  `db:"subscription_ends_at"`
	SubscriptionStatus       string         `db:"subscription_status"`
	SetupCompletedAt         sql.NullInt64  `db:"setup_completed_at"`
	WhatsAppBusinessPhone    sql.NullString `db:"whatsapp_business_phone"`
	WhatsAppBusinessTokenEnc []byte         `db:"whatsapp_business_token_enc"`
	WhatsAppConnectedAt      sql.NullInt64  `db:"whatsapp_connected_at"`
	KioskSettings            string         `db:"kiosk_settings"`
	ChargeSettings           string         `db:"charge_settings"`
}

func gymToRow(g *gymDomain.Gym) sqliteGymRow {
	pm, _ := json.Marshal(g.PaymentMethods)
	ks, _ := json.Marshal(g.KioskSettings)
	chargeMap := g.ChargeSettings
	if chargeMap == nil {
		chargeMap = map[string]any{}
	}
	cs, _ := json.Marshal(chargeMap)
	return sqliteGymRow{
		ID:                       g.ID.String(),
		GymID:                    g.ID.String(),
		Version:                  g.Version,
		CreatedAt:                g.CreatedAt.UnixMilli(),
		UpdatedAt:                g.UpdatedAt.UnixMilli(),
		DeletedAt:                nullableMs(g.DeletedAt),
		Name:                     nullableString(g.Name),
		City:                     nullableString(g.City),
		WhatsApp:                 nullableString(g.WhatsApp),
		Country:                  g.Country,
		Timezone:                 g.Timezone,
		RFC:                      nullableString(g.RFC),
		RazonSocial:              nullableString(g.RazonSocial),
		CodigoPostal:             nullableString(g.CodigoPostal),
		RegimenFiscal:            nullableString(g.RegimenFiscal),
		LogoURL:                  nullableString(g.LogoURL),
		PrimaryColor:             nullableString(g.PrimaryColor),
		SecondaryColor:           nullableString(g.SecondaryColor),
		PaymentMethods:           string(pm),
		OpenTime:                 nullableString(g.OpenTime),
		CloseTime:                nullableString(g.CloseTime),
		SubscriptionPlan:         g.SubscriptionPlan,
		TrialEndsAt:              nullableMs(g.TrialEndsAt),
		SubscriptionEndsAt:       nullableMs(g.SubscriptionEndsAt),
		SubscriptionStatus:       g.SubscriptionStatus,
		SetupCompletedAt:         nullableMs(g.SetupCompletedAt),
		WhatsAppBusinessPhone:    nullableString(g.WhatsAppBusinessPhone),
		WhatsAppBusinessTokenEnc: g.WhatsAppBusinessTokenEnc,
		WhatsAppConnectedAt:      nullableMs(g.WhatsAppConnectedAt),
		KioskSettings:            string(ks),
		ChargeSettings:           string(cs),
	}
}

func gymFromRow(r *sqliteGymRow) *gymDomain.Gym {
	id, _ := uuid.Parse(r.ID)
	var pm []string
	_ = json.Unmarshal([]byte(r.PaymentMethods), &pm)
	if pm == nil {
		pm = []string{}
	}
	var ks map[string]any
	_ = json.Unmarshal([]byte(r.KioskSettings), &ks)
	var cs map[string]any
	if r.ChargeSettings != "" {
		_ = json.Unmarshal([]byte(r.ChargeSettings), &cs)
	}
	if cs == nil {
		cs = map[string]any{}
	}
	g := &gymDomain.Gym{
		ID:                       id,
		Version:                  r.Version,
		Country:                  r.Country,
		Timezone:                 r.Timezone,
		PaymentMethods:           pm,
		SubscriptionPlan:         r.SubscriptionPlan,
		SubscriptionStatus:       r.SubscriptionStatus,
		WhatsAppBusinessTokenEnc: r.WhatsAppBusinessTokenEnc,
		KioskSettings:            ks,
		ChargeSettings:           cs,
		CreatedAt:                time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:                time.UnixMilli(r.UpdatedAt).UTC(),
	}
	g.Name = nullToPtr(r.Name)
	g.City = nullToPtr(r.City)
	g.WhatsApp = nullToPtr(r.WhatsApp)
	g.RFC = nullToPtr(r.RFC)
	g.RazonSocial = nullToPtr(r.RazonSocial)
	g.CodigoPostal = nullToPtr(r.CodigoPostal)
	g.RegimenFiscal = nullToPtr(r.RegimenFiscal)
	g.LogoURL = nullToPtr(r.LogoURL)
	g.PrimaryColor = nullToPtr(r.PrimaryColor)
	g.SecondaryColor = nullToPtr(r.SecondaryColor)
	g.OpenTime = nullToPtr(r.OpenTime)
	g.CloseTime = nullToPtr(r.CloseTime)
	g.WhatsAppBusinessPhone = nullToPtr(r.WhatsAppBusinessPhone)
	g.TrialEndsAt = nullMsToTime(r.TrialEndsAt)
	g.SubscriptionEndsAt = nullMsToTime(r.SubscriptionEndsAt)
	g.SetupCompletedAt = nullMsToTime(r.SetupCompletedAt)
	g.WhatsAppConnectedAt = nullMsToTime(r.WhatsAppConnectedAt)
	g.DeletedAt = nullMsToTime(r.DeletedAt)
	return g
}

func (r *GymSQLiteRepository) Create(tx sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := gymToRow(g)
	const stmt = `
		INSERT INTO gyms (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    name, city, whatsapp, country, timezone,
		    rfc, razon_social, codigo_postal, regimen_fiscal,
		    logo_url, primary_color, secondary_color,
		    payment_methods, open_time, close_time,
		    subscription_plan, trial_ends_at, subscription_ends_at, subscription_status, setup_completed_at,
		    whatsapp_business_phone, whatsapp_business_token_enc, whatsapp_connected_at,
		    kiosk_settings, charge_settings
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :name, :city, :whatsapp, :country, :timezone,
		    :rfc, :razon_social, :codigo_postal, :regimen_fiscal,
		    :logo_url, :primary_color, :secondary_color,
		    :payment_methods, :open_time, :close_time,
		    :subscription_plan, :trial_ends_at, :subscription_ends_at, :subscription_status, :setup_completed_at,
		    :whatsapp_business_phone, :whatsapp_business_token_enc, :whatsapp_connected_at,
		    :kiosk_settings, :charge_settings
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueGym(stx, g); err != nil {
		return nil, err
	}
	return g, nil
}

func (r *GymSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*gymDomain.Gym, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteGymRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM gyms WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(gymErrors.ErrGymNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return gymFromRow(&row), nil
}

func (r *GymSQLiteRepository) Update(tx sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	g.UpdatedAt = time.Now().UTC()
	row := gymToRow(g)
	const stmt = `
		UPDATE gyms SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    name = :name, city = :city, whatsapp = :whatsapp, country = :country, timezone = :timezone,
		    rfc = :rfc, razon_social = :razon_social, codigo_postal = :codigo_postal, regimen_fiscal = :regimen_fiscal,
		    logo_url = :logo_url, primary_color = :primary_color, secondary_color = :secondary_color,
		    payment_methods = :payment_methods, open_time = :open_time, close_time = :close_time,
		    subscription_plan = :subscription_plan, trial_ends_at = :trial_ends_at,
		    subscription_ends_at = :subscription_ends_at, subscription_status = :subscription_status,
		    setup_completed_at = :setup_completed_at,
		    whatsapp_business_phone = :whatsapp_business_phone,
		    whatsapp_business_token_enc = :whatsapp_business_token_enc,
		    whatsapp_connected_at = :whatsapp_connected_at,
		    kiosk_settings = :kiosk_settings,
		    charge_settings = :charge_settings
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueGym(stx, g); err != nil {
		return nil, err
	}
	return g, nil
}

// ExistsByWhatsApp — defense en profundidad en el sidecar: aunque el cloud
// es la fuente de verdad para la unicidad (uq_gyms_whatsapp en Postgres),
// también enforcement local para que si la fila duplicada se cuela por sync
// el repo no quede inconsistente. excludeGymID = uuid.Nil = "no excluir".
func (r *GymSQLiteRepository) ExistsByWhatsApp(tx sharedDomain.Transaction, whatsapp string, excludeGymID uuid.UUID) (bool, error) {
	if whatsapp == "" {
		return false, nil
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	if excludeGymID == uuid.Nil {
		err := stx.Get(context.Background(), &n,
			`SELECT COUNT(1) FROM gyms WHERE whatsapp = ? AND deleted_at IS NULL`,
			whatsapp)
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM gyms WHERE whatsapp = ? AND deleted_at IS NULL AND id <> ?`,
		whatsapp, excludeGymID.String())
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *GymSQLiteRepository) HasMembershipType(tx sharedDomain.Transaction, gymID uuid.UUID) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM membership_types WHERE gym_id = ? AND deleted_at IS NULL`, gymID.String())
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func enqueueGym(stx *sharedDomain.SqlxTransaction, g *gymDomain.Gym) error {
	if stx.Queue == nil {
		return nil
	}
	chargeSettings := g.ChargeSettings
	if chargeSettings == nil {
		chargeSettings = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{
		"id":                  g.ID.String(),
		"gym_id":              g.ID.String(),
		"version":             g.Version,
		"name":                g.Name,
		"city":                g.City,
		"whatsapp":            g.WhatsApp,
		"country":             g.Country,
		"timezone":            g.Timezone,
		"payment_methods":     g.PaymentMethods,
		"subscription_plan":   g.SubscriptionPlan,
		"subscription_status": g.SubscriptionStatus,
		"trial_ends_at":       g.TrialEndsAt,
		"setup_completed_at":  g.SetupCompletedAt,
		"charge_settings":     chargeSettings,
		"updated_at":          g.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "gyms", g.ID.String(), "upsert", payload, g.Version)
}
