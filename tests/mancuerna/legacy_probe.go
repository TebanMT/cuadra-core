//go:build server

// Runs against the harness's disposable Unix-socket database only.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"os"
	"time"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func main() {
	dsn := os.Getenv("MANCUERNA_LEGACY_DSN")
	if dsn == "" {
		panic("MANCUERNA_LEGACY_DSN required")
	}
	db, e := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(e)
	pool, e := db.DB()
	must(e)
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	u, e := domain.NewTenantPostgresUnitOfWork(db)
	must(e)
	a := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	b := uuid.MustParse("10000000-0000-4000-8000-000000000002")
	for i := 0; i < 20; i++ {
		for _, id := range []uuid.UUID{a, uuid.Nil, b} {
			ctx := context.Background()
			if id != uuid.Nil {
				ctx = domain.WithTenant(ctx, id)
			}
			tx, e := u.Query(ctx)
			must(e)
			var count int64
			must(tx.(*domain.GormTransaction).Tx.Raw("SELECT count(*) FROM products").Scan(&count).Error)
			domain.CloseQuery(tx)
			want := int64(1)
			if id == uuid.Nil {
				want = 0
			}
			if count != want {
				panic("legacy tenant isolation failed")
			}
		}
	}
	var count int64
	if db.Raw("SELECT count(*) FROM mancuerna.accounts").Scan(&count).Error == nil {
		panic("legacy role gained personal access")
	}
	admin := auth.NewJWTService("synthetic-admin-key-for-s00-only")
	administrative, e := admin.GenerateAccessToken(uuid.New(), a, "owner")
	must(e)
	_, e = admin.ValidateAccessToken(administrative)
	must(e)
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	must(e)
	claims := jwt.MapClaims{"sub": uuid.NewString(), "aud": "mancuerna", "iss": "tinta-personal", "exp": time.Now().Add(time.Minute).Unix(), "sid": uuid.NewString(), "did": uuid.NewString()}
	personal, e := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(key)
	must(e)
	if _, e = admin.ValidateAccessToken(personal); e == nil {
		panic("personal token accepted by admin")
	}
	validate := func(raw string) error {
		_, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) { return pub, nil }, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience("mancuerna"), jwt.WithIssuer("tinta-personal"), jwt.WithExpirationRequired())
		return err
	}
	must(validate(personal))
	if validate(administrative) == nil {
		panic("admin token accepted by personal verifier")
	}
	for field, value := range map[string]any{"aud": "dashboard", "iss": "other", "exp": time.Now().Add(-time.Hour).Unix()} {
		bad := jwt.MapClaims{}
		for k, v := range claims {
			bad[k] = v
		}
		bad[field] = value
		s, e := jwt.NewWithClaims(jwt.SigningMethodEdDSA, bad).SignedString(key)
		must(e)
		if validate(s) == nil {
			panic("invalid personal claim accepted")
		}
	}
	fmt.Println("legacy UoW: 60 pooled reads, no personal privileges; current auth + isolated personal verifier: PASS")
}
