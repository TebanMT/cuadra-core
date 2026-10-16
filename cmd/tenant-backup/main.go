//go:build server

// tenant-backup never reads application .env files. Credentials are supplied
// explicitly; restored data is restricted to an empty local recovery database.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	infra "github.com/cuadra/cuadra-core/infraestructure/db"
	"github.com/cuadra/cuadra-core/src/shared/tenantbackup"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tenant-backup:", err)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "export", "export, restore or verify")
	gym := flag.String("gym", "", "gym UUID for export")
	path := flag.String("file", "", "encrypted archive path")
	keyPath := flag.String("key-file", "", "file containing 32 random bytes; keep separately")
	migrations := flag.String("migrations", "db_migrations/postgres", "schema directory for a fresh restore target")
	flag.Parse()
	if *path == "" || *keyPath == "" {
		return fmt.Errorf("file and key-file required")
	}
	key, err := os.ReadFile(*keyPath)
	if err != nil {
		return fmt.Errorf("read key file: %w", err)
	}
	if len(key) != 32 {
		return fmt.Errorf("key file must contain exactly 32 bytes")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("explicit DATABASE_URL required")
	}
	if *mode == "restore" {
		u, err := url.Parse(dsn)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") || !strings.HasPrefix(u.Path, "/tinta_restore_") {
			return fmt.Errorf("restore requires localhost/tinta_restore_* database")
		}
		for option := range u.Query() {
			if option != "sslmode" && option != "connect_timeout" {
				return fmt.Errorf("unsupported restore connection option")
			}
		}
	} else if *mode != "export" && *mode != "verify" {
		return fmt.Errorf("unknown mode")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return fmt.Errorf("database connection failed")
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var summary tenantbackup.Summary
	if *mode == "export" {
		id, err := uuid.Parse(*gym)
		if err != nil {
			return err
		}
		data, s, err := tenantbackup.Export(ctx, db, id, key)
		if err != nil {
			return err
		}
		file, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		if _, err = file.Write(data); err != nil {
			file.Close()
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		summary = s
	} else {
		if *mode == "restore" {
			if err := infra.ApplyPostgresMigrations(db, *migrations); err != nil {
				return err
			}
		}
		data, err := os.ReadFile(*path)
		if err != nil {
			return err
		}
		if *mode == "restore" {
			summary, err = tenantbackup.Restore(ctx, db, data, key)
		} else {
			summary, err = tenantbackup.Verify(ctx, db, data, key)
		}
		if err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(summary)
}
