//go:build server

// Package main — `make seed` entry point.
//
// Inserts a canonical demo gym with members, payments, products, and check-ins
// covering every dashboard / attention-required edge case (active, expiring,
// expired-recoverable, lost, low stock, pending balance, birthday today).
// Idempotent on a clean DB; not safe to re-run on a populated one — by design,
// since the goal is reproducible local fixtures, not migrations.
package main

import (
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
		dsn = "postgresql://cuadra:cuadra_dev@localhost:5432/cuadra?sslmode=disable"
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

	// Membership types
	planMonthlyID := uuid.New()
	planQuarterID := uuid.New()
	if err := tx.Create(&[]memModels.MembershipTypeModel{
		{
			ID: planMonthlyID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Name: "Mensual", Price: 500, DurationDays: 30, Active: true,
		},
		{
			ID: planQuarterID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Name: "Trimestral", Price: 1300, DurationDays: 90, Active: true,
		},
	}).Error; err != nil {
		return fmt.Errorf("membership_types: %w", err)
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
		if err := tx.Create(&memModels.MemberModel{
			ID: memberID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Folio:    fmt.Sprintf("MEM-%06d", time.Now().UnixNano()%1000000),
			FullName: sm.Name, Phone: sm.Phone, Status: sm.Status,
			Birthdate: sm.Birthdate, EnrollmentPaid: true,
			CreatedBy: ownerID,
		}).Error; err != nil {
			return fmt.Errorf("member %s: %w", sm.Name, err)
		}

		startDate := today.AddDate(0, 0, sm.StartOffset)
		expiryDate := today.AddDate(0, 0, sm.ExpiryOffset)
		membershipID := uuid.New()
		if err := tx.Create(&memModels.MembershipModel{
			ID: membershipID, GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			MemberID: memberID, MembershipTypeID: planMonthlyID,
			TypeNameSnapshot: "Mensual", PriceSnapshot: 500, DurationDaysSnapshot: 30,
			StartDate: startDate, ExpiryDate: expiryDate, Status: "active",
		}).Error; err != nil {
			return fmt.Errorf("membership %s: %w", sm.Name, err)
		}

		// Payment when membership started (counts toward income).
		if err := tx.Create(&billingModels.PaymentModel{
			ID: uuid.New(), GymID: gymID, Version: 1, CreatedAt: startDate, UpdatedAt: startDate,
			Folio:    fmt.Sprintf("PAGO/%d", time.Now().UnixNano()%100000000),
			MemberID: &memberID,
			Amount:   500, PaymentMethod: "cash", Concept: "membership",
			BalancePending: sm.BalancePending,
			PaymentDate:    startDate, OperatorID: operatorID,
		}).Error; err != nil {
			return fmt.Errorf("payment %s: %w", sm.Name, err)
		}

		if sm.LastCheckin != nil {
			ts := today.AddDate(0, 0, -*sm.LastCheckin).Add(9 * time.Hour)
			if err := tx.Create(&chkModels.CheckinModel{
				ID: uuid.New(), GymID: gymID, Version: 1, CreatedAt: ts, UpdatedAt: ts,
				MemberID: memberID, CheckinAt: ts, Method: "manual", Result: "allowed_active",
				OperatorID: &operatorID,
			}).Error; err != nil {
				return fmt.Errorf("checkin %s: %w", sm.Name, err)
			}
		}
	}

	// Today's cash payment (boosts dashboard "caja del día").
	if err := tx.Create(&billingModels.PaymentModel{
		ID: uuid.New(), GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
		Folio:  fmt.Sprintf("PAGO/HOY/%d", time.Now().UnixNano()%1000000),
		Amount: 350, PaymentMethod: "cash", Concept: "product",
		PaymentDate: today, OperatorID: operatorID,
	}).Error; err != nil {
		return fmt.Errorf("today payment: %w", err)
	}

	// Products — one with low stock, one healthy.
	if err := tx.Create(&[]prodModels.ProductModel{
		{
			ID: uuid.New(), GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Name: "Agua Ciel 1L", Price: 25, Stock: 2, StockMinimum: 10, Active: true,
		},
		{
			ID: uuid.New(), GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
			Name: "Proteína sabor chocolate", Price: 950, Stock: 12, StockMinimum: 5, Active: true,
		},
	}).Error; err != nil {
		return fmt.Errorf("products: %w", err)
	}

	fmt.Printf("seed: gym_id=%s owner=owner@cuadra.demo password=ClaveSeed!2026\n", gymID)
	return nil
}
