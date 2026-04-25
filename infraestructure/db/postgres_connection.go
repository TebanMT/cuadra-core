//go:build server

package db

import (
	"log"
	"sync"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
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
		postgresInstance, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
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
