//go:build server

// Package main — `make seed` entry point.
//
// Inserts a canonical demo gym with members, payments, products, and check-ins
// covering every dashboard / attention-required edge case (active, expiring,
// expired-recoverable, lost, low stock, pending balance, birthday today).
// Idempotent on a clean DB; not safe to re-run on a populated one — by design,
// since the goal is reproducible local fixtures, not migrations.
//
// Cada fila de dominio se ESPEJA a sync_entities (patrón emitSyncEntity de
// cmd/import-gym). Sin el espejo, un sidecar fresco que hace full-sync contra
// la DB seedeada ve un gym VACÍO — el dev recrea "Mensual" a mano en el
// desktop y fabrica justo la colisión de unique index (23505) que en prod es
// un caso raro de multi-device.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"
	billingModels "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/models"
	chkModels "github.com/cuadra/cuadra-core/src/modules/checkins/infraestructure/db/models"
	gymModels "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/models"
	memModels "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/models"
	prodModels "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/models"
	usersModels "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/models"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgresql://tinta:tinta_dev@localhost:5432/tinta?sslmode=disable"
	}
	db := infraDB.InitPostgres(dsn)
	defer infraDB.ClosePostgres()

	if err := infraDB.ApplyPostgresMigrations(db, "db_migrations/postgres"); err != nil {
		log.Fatalf("apply migrations: %v", err)
	}

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return seed(tx, now, today)
	}); err != nil {
		log.Fatalf("seed: %v", err)
	}
	fmt.Println("seed: ok")
}

func seed(tx *gorm.DB, now, today time.Time) error {
	gymID := uuid.New()
	ownerID := uuid.New()
	operatorID := uuid.New()

	// Gym
	gymName := "Gym Bros San Miguel"
	city := "San Miguel de Allende"
	wa := "+5214151234567"
	if err := tx.Create(&gymModels.GymModel{
		ID:                 gymID,
		GymID:              gymID,
		Version:            1,
		CreatedAt:          now,
		UpdatedAt:          now,
		Name:               &gymName,
		City:               &city,
		WhatsApp:           &wa,
		Country:            "MX",
		Timezone:           "America/Mexico_City",
		PaymentMethods:     `["cash","transfer","card"]`,
		KioskSettings:      `{}`,
		SubscriptionPlan:   "trial",
		SubscriptionStatus: "active",
		SetupCompletedAt:   &now,
	}).Error; err != nil {
		return fmt.Errorf("gym: %w", err)
	}
	// Los campos cloud-owned del gym (subscription_*, trial_ends_at, ...) se
	// omiten del payload a propósito: gymCanonicalAugmentExpr los inyecta
	// desde el row vivo en cada pull/full-sync (server_store.go).
	if err := emitSyncEntity(tx, gymID, gymID, "gyms", 1, now, map[string]any{
		"created_at":         now.UnixMilli(),
		"updated_at":         now.UnixMilli(),
		"name":               gymName,
		"city":               city,
		"whatsapp":           wa,
		"country":            "MX",
		"timezone":           "America/Mexico_City",
		"payment_methods":    []string{"cash", "transfer", "card"},
		"setup_completed_at": now.UnixMilli(),
		"charge_settings":    map[string]any{},
		"kiosk_settings":     map[string]any{},
	}); err != nil {
		return fmt.Errorf("emit gym: %w", err)
	}

	pwHash, _ := bcrypt.GenerateFromPassword([]byte("ClaveSeed!2026"), 12)
	if err := tx.Create(&[]usersModels.UserModel{
		{
			ID: ownerID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Email: "owner@cuadra.demo", PasswordHash: string(pwHash),
			FullName: "Esteban Owner", Role: "owner", Active: true,
		},
		{
			ID: operatorID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Email: "ops@cuadra.demo", PasswordHash: string(pwHash),
			FullName: "Recepción", Role: "operator", Active: true,
			CreatedBy: &ownerID,
		},
	}).Error; err != nil {
		return fmt.Errorf("users: %w", err)
	}
	for _, u := range []struct {
		id          uuid.UUID
		email, name string
		role        string
	}{
		{ownerID, "owner@cuadra.demo", "Esteban Owner", "owner"},
		{operatorID, "ops@cuadra.demo", "Recepción", "operator"},
	} {
		// Mismo shape que enqueueUser (user_sqlite.go): todas las columnas
		// NOT NULL presentes; password_hash viaja para que el login offline
		// del desktop funcione contra el fixture.
		if err := emitSyncEntity(tx, gymID, u.id, "users", 1, now, map[string]any{
			"created_at":           now.UnixMilli(),
			"updated_at":           now.UnixMilli(),
			"email":                u.email,
			"password_hash":        string(pwHash),
			"full_name":            u.name,
			"phone":                "",
			"role":                 u.role,
			"active":               true,
			"must_change_password": false,
			"pin_hash":             nil,
			"pin_assigned_at":      nil,
		}); err != nil {
			return fmt.Errorf("emit user %s: %w", u.email, err)
		}
	}

	// Membership types
	planMonthlyID := uuid.New()
	planQuarterID := uuid.New()
	oneMonth, threeMonths := 1, 3
	if err := tx.Create(&[]memModels.MembershipTypeModel{
		{
			ID: planMonthlyID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Name: "Mensual", Price: 500, DurationDays: 30, DurationMonths: &oneMonth, Active: true,
		},
		{
			ID: planQuarterID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Name: "Trimestral", Price: 1300, DurationDays: 90, DurationMonths: &threeMonths, Active: true,
		},
	}).Error; err != nil {
		return fmt.Errorf("membership_types: %w", err)
	}
	for _, p := range []struct {
		id     uuid.UUID
		name   string
		price  float64
		days   int
		months int
	}{
		{planMonthlyID, "Mensual", 500, 30, 1},
		{planQuarterID, "Trimestral", 1300, 90, 3},
	} {
		if err := emitSyncEntity(tx, gymID, p.id, "membership_types", 1, now, map[string]any{
			"created_at":      now.UnixMilli(),
			"updated_at":      now.UnixMilli(),
			"name":            p.name,
			"price":           p.price,
			"duration_days":   p.days,
			"duration_months": p.months,
			"enrollment_fee":  0,
			"maintenance_fee": 0,
			"active":          true,
		}); err != nil {
			return fmt.Errorf("emit membership_type %s: %w", p.name, err)
		}
	}

	// Members spanning every dashboard category.
	type seedMember struct {
		Name           string
		Phone          string
		Status         string
		StartOffset    int  // days before today the membership started
		ExpiryOffset   int  // days from today until expiry (negative = expired)
		LastCheckin    *int // days ago; nil = no checkins
		Birthdate      *time.Time
		BalancePending float64
	}
	bdayToday := time.Date(1990, today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	bdayOther := time.Date(1985, 6, 15, 0, 0, 0, 0, time.UTC)
	five := 5
	thirty := 30
	roster := []seedMember{
		{Name: "Juan Pérez", Phone: "5215551001001", Status: "active", StartOffset: -10, ExpiryOffset: 20, LastCheckin: &five, Birthdate: &bdayOther},
		{Name: "María López", Phone: "5215551001002", Status: "active", StartOffset: -25, ExpiryOffset: 5, LastCheckin: &thirty}, // expiring soon + inactive_involuntary
		{Name: "Pedro Gómez", Phone: "5215551001003", Status: "active", StartOffset: -90, ExpiryOffset: -10},                     // recoverable
		{Name: "Lucía Ruiz", Phone: "5215551001004", Status: "lost", StartOffset: -120, ExpiryOffset: -45},                       // lost (excluded from recoverable)
		{Name: "Andrés Solís", Phone: "5215551001005", Status: "active", StartOffset: -5, ExpiryOffset: 25, LastCheckin: &five, Birthdate: &bdayToday, BalancePending: 250},
		{Name: "Patricia Vega", Phone: "5215551001006", Status: "active", StartOffset: -35, ExpiryOffset: 1, LastCheckin: &five}, // expiring tomorrow
	}

	for _, sm := range roster {
		memberID := uuid.New()
		memberFolio := fmt.Sprintf("MEM-%06d", time.Now().UnixNano()%1000000)
		if err := tx.Create(&memModels.MemberModel{
			ID: memberID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Folio:    memberFolio,
			FullName: sm.Name, Phone: sm.Phone, Status: sm.Status,
			Birthdate: sm.Birthdate, EnrollmentPaid: true,
			CreatedBy: ownerID,
		}).Error; err != nil {
			return fmt.Errorf("member %s: %w", sm.Name, err)
		}
		var bd any
		if sm.Birthdate != nil {
			bd = dateStr(*sm.Birthdate)
		}
		if err := emitSyncEntity(tx, gymID, memberID, "members", 1, now, map[string]any{
			"created_at":      now.UnixMilli(),
			"updated_at":      now.UnixMilli(),
			"folio":           memberFolio,
			"full_name":       sm.Name,
			"phone":           sm.Phone,
			"birthdate":       bd,
			"status":          sm.Status,
			"enrollment_paid": true,
			"created_by":      ownerID.String(),
		}); err != nil {
			return fmt.Errorf("emit member %s: %w", sm.Name, err)
		}

		startDate := today.AddDate(0, 0, sm.StartOffset)
		expiryDate := today.AddDate(0, 0, sm.ExpiryOffset)
		membershipID := uuid.New()
		if err := tx.Create(&memModels.MembershipModel{
			ID: membershipID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			MemberID: memberID, MembershipTypeID: planMonthlyID,
			TypeNameSnapshot: "Mensual", PriceSnapshot: 500, DurationDaysSnapshot: 30,
			DurationMonthsSnapshot: &oneMonth,
			StartDate:              startDate, ExpiryDate: &expiryDate, Status: "active",
		}).Error; err != nil {
			return fmt.Errorf("membership %s: %w", sm.Name, err)
		}
		if err := emitSyncEntity(tx, gymID, membershipID, "memberships", 1, now, map[string]any{
			"created_at":               now.UnixMilli(),
			"updated_at":               now.UnixMilli(),
			"member_id":                memberID.String(),
			"membership_type_id":       planMonthlyID.String(),
			"type_name_snapshot":       "Mensual",
			"price_snapshot":           500,
			"duration_days_snapshot":   30,
			"duration_months_snapshot": 1,
			"start_date":               dateStr(startDate),
			"expiry_date":              dateStr(expiryDate),
			"status":                   "active",
		}); err != nil {
			return fmt.Errorf("emit membership %s: %w", sm.Name, err)
		}

		// Payment when membership started (counts toward income).
		paymentID := uuid.New()
		paymentFolio := fmt.Sprintf("PAGO/%d", time.Now().UnixNano()%100000000)
		if err := tx.Create(&billingModels.PaymentModel{
			ID: paymentID, GymID: gymID, Version: 1, CreatedAt: startDate, UpdatedAt: startDate,
			Folio:    paymentFolio,
			MemberID: &memberID,
			Amount:   500, PaymentMethod: "cash", Concept: "membership",
			BalancePending: sm.BalancePending,
			PaymentDate:    startDate, OperatorID: operatorID,
		}).Error; err != nil {
			return fmt.Errorf("payment %s: %w", sm.Name, err)
		}
		if err := emitSyncEntity(tx, gymID, paymentID, "payments", 1, now, map[string]any{
			"created_at":      startDate.UnixMilli(),
			"updated_at":      startDate.UnixMilli(),
			"folio":           paymentFolio,
			"member_id":       memberID.String(),
			"amount":          500,
			"payment_method":  "cash",
			"concept":         "membership",
			"discount_amount": 0,
			"balance_pending": sm.BalancePending,
			"payment_date":    dateStr(startDate),
			"operator_id":     operatorID.String(),
		}); err != nil {
			return fmt.Errorf("emit payment %s: %w", sm.Name, err)
		}

		if sm.LastCheckin != nil {
			ts := today.AddDate(0, 0, -*sm.LastCheckin).Add(9 * time.Hour)
			checkinID := uuid.New()
			if err := tx.Create(&chkModels.CheckinModel{
				ID: checkinID, GymID: gymID, Version: 1, CreatedAt: ts, UpdatedAt: ts,
				MemberID: memberID, CheckinAt: ts, Method: "manual", Result: "allowed_active",
				OperatorID: &operatorID,
			}).Error; err != nil {
				return fmt.Errorf("checkin %s: %w", sm.Name, err)
			}
			if err := emitSyncEntity(tx, gymID, checkinID, "checkins", 1, now, map[string]any{
				"created_at":      ts.UnixMilli(),
				"updated_at":      ts.UnixMilli(),
				"member_id":       memberID.String(),
				"checkin_at":      ts.UnixMilli(),
				"method":          "manual",
				"result":          "allowed_active",
				"operator_id":     operatorID.String(),
				"manual_override": false,
			}); err != nil {
				return fmt.Errorf("emit checkin %s: %w", sm.Name, err)
			}
		}
	}

	// Today's cash payment (boosts dashboard "caja del día").
	todayPaymentID := uuid.New()
	todayPaymentFolio := fmt.Sprintf("PAGO/HOY/%d", time.Now().UnixNano()%1000000)
	if err := tx.Create(&billingModels.PaymentModel{
		ID: todayPaymentID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
		Folio:  todayPaymentFolio,
		Amount: 350, PaymentMethod: "cash", Concept: "product",
		PaymentDate: today, OperatorID: operatorID,
	}).Error; err != nil {
		return fmt.Errorf("today payment: %w", err)
	}
	if err := emitSyncEntity(tx, gymID, todayPaymentID, "payments", 1, now, map[string]any{
		"created_at":      now.UnixMilli(),
		"updated_at":      now.UnixMilli(),
		"folio":           todayPaymentFolio,
		"amount":          350,
		"payment_method":  "cash",
		"concept":         "product",
		"discount_amount": 0,
		"balance_pending": 0,
		"payment_date":    dateStr(today),
		"operator_id":     operatorID.String(),
	}); err != nil {
		return fmt.Errorf("emit today payment: %w", err)
	}

	// Products — one with low stock, one healthy.
	waterID := uuid.New()
	proteinID := uuid.New()
	if err := tx.Create(&[]prodModels.ProductModel{
		{
			ID: waterID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Name: "Agua Ciel 1L", Price: 25, Stock: 2, StockMinimum: 10, Active: true,
		},
		{
			ID: proteinID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Name: "Proteína sabor chocolate", Price: 950, Stock: 12, StockMinimum: 5, Active: true,
		},
	}).Error; err != nil {
		return fmt.Errorf("products: %w", err)
	}
	for _, p := range []struct {
		id         uuid.UUID
		name       string
		price      float64
		stock, min int
	}{
		{waterID, "Agua Ciel 1L", 25, 2, 10},
		{proteinID, "Proteína sabor chocolate", 950, 12, 5},
	} {
		if err := emitSyncEntity(tx, gymID, p.id, "products", 1, now, map[string]any{
			"created_at":    now.UnixMilli(),
			"updated_at":    now.UnixMilli(),
			"name":          p.name,
			"price":         p.price,
			"stock":         p.stock,
			"stock_minimum": p.min,
			"active":        true,
		}); err != nil {
			return fmt.Errorf("emit product %s: %w", p.name, err)
		}
	}

	fmt.Printf("seed: gym_id=%s owner=owner@cuadra.demo password=ClaveSeed!2026\n", gymID)
	fmt.Printf("seed: sync_entities espejadas=%d (sidecars frescos ven el fixture vía full-sync)\n", syncEmitSeq)
	return nil
}

// ── Espejo a sync_entities ──────────────────────────────────────────────
//
// Patrón copiado de cmd/import-gym (la referencia canónica de "dominio →
// journal"). Se duplica a propósito: ambos son binarios main autocontenidos
// y compartir helpers acoplaría el fixture de dev al importador real.

// syncEmitSeq desempata server_updated_at entre filas del mismo rank
// topológico (1µs por emisión) para que el pull pagine determinista.
// De paso sirve de conteo para el log final.
var syncEmitSeq int

// syncTopoRank ordena server_updated_at por capas de FK (mismo criterio que
// SyncedTables): el sidecar aplica padres antes que hijos aunque la página
// del pull caiga a la mitad de la cadena.
func syncTopoRank(entityType string) int {
	switch entityType {
	case "gyms", "users":
		return 0
	case "membership_types", "products":
		return 1
	case "members":
		return 2
	case "memberships", "sales":
		return 3
	case "payments", "checkins", "cash_close_events":
		return 4
	case "sale_items", "stock_movements":
		return 5
	default:
		return 6
	}
}

// emitSyncEntity upserts una fila (gym_id, entity_type, entity_id) en
// sync_entities con el payload + version del dominio recién creado. El wire
// format espeja al de los enqueue del sidecar / import-gym: timestamps en
// epoch-ms, fechas date-only como "YYYY-MM-DD", dinero en PESOS (el apply
// del sidecar convierte a centavos al aterrizar en SQLite).
func emitSyncEntity(tx *gorm.DB, gymID, entityID uuid.UUID, entityType string, version int, now time.Time, payload map[string]any) error {
	// Defense in depth: pin id + gym_id desde el call site, ignorando lo que
	// traiga el mapa.
	payload["id"] = entityID.String()
	payload["gym_id"] = gymID.String()
	payload["version"] = version
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	syncEmitSeq++
	serverUpdatedAt := now.
		Add(time.Duration(syncTopoRank(entityType)) * time.Second).
		Add(time.Duration(syncEmitSeq) * time.Microsecond)
	return tx.Exec(`
		INSERT INTO sync_entities (gym_id, entity_type, entity_id, version, payload, server_updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?::jsonb, ?, NULL)
		ON CONFLICT (gym_id, entity_type, entity_id) DO UPDATE
		SET version = EXCLUDED.version,
		    payload = EXCLUDED.payload,
		    server_updated_at = EXCLUDED.server_updated_at,
		    deleted_at = EXCLUDED.deleted_at`,
		gymID, entityType, entityID, version, string(b), serverUpdatedAt,
	).Error
}

func dateStr(t time.Time) string { return t.UTC().Format("2006-01-02") }
