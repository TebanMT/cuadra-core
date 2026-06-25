//go:build server

package db

import (
	"log"
	"os"
	"sync"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	postgresInstance *gorm.DB
	postgresOnce     sync.Once
)

// InitPostgres opens (once) the cloud Postgres connection. We let GORM manage
// pooling but cap the upper bound — gym traffic is low and Hetzner Postgres
// has a smaller default than RDS.
func InitPostgres(dsn string) *gorm.DB {
	postgresOnce.Do(func() {
		var err error
		// IgnoreRecordNotFoundError: optional lookups (per-gym template
		// overrides, idempotency probes) use First() and treat
		// ErrRecordNotFound as "no override / not seen yet" — it's expected
		// flow, not an error worth logging. We keep Warn level so slow
		// queries and real errors still surface.
		gormCfg := &gorm.Config{
			Logger: logger.New(
				log.New(os.Stdout, "", log.LstdFlags),
				logger.Config{
					SlowThreshold:             200 * time.Millisecond,
					LogLevel:                  logger.Warn,
					IgnoreRecordNotFoundError: true,
				},
			),
		}
		postgresInstance, err = gorm.Open(postgres.Open(dsn), gormCfg)
		if err != nil {
			log.Fatalf("postgres connect: %v", err)
		}
		sqlDB, err := postgresInstance.DB()
		if err != nil {
			log.Fatalf("postgres handle: %v", err)
		}
		sqlDB.SetMaxIdleConns(5)
		sqlDB.SetMaxOpenConns(20)
		sqlDB.SetConnMaxLifetime(time.Hour)
	})
	return postgresInstance
}

func ClosePostgres() {
	if postgresInstance == nil {
		return
	}
	if sqlDB, err := postgresInstance.DB(); err == nil {
		_ = sqlDB.Close()
	}
}
