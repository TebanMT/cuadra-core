//go:build server

// migrate separates schema ownership from the restricted application role.
package main

import (
	"flag"
	"fmt"
	"os"

	infra "github.com/cuadra/cuadra-core/infraestructure/db"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	dir := flag.String("dir", "db_migrations/postgres", "migration directory")
	appRole := flag.String("app-role", os.Getenv("TINTA_APP_DB_ROLE"), "existing restricted runtime role to grant access after migration")
	flag.Parse()
	if os.Getenv("DATABASE_URL") == "" {
		fmt.Fprintln(os.Stderr, "explicit DATABASE_URL required")
		os.Exit(1)
	}
	db, err := gorm.Open(postgres.Open(os.Getenv("DATABASE_URL")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "database connection failed")
		os.Exit(1)
	}
	if err = infra.ApplyPostgresMigrations(db, *dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *appRole != "" {
		if err = infra.GrantRuntimeRole(db, *appRole); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()
	fmt.Println("Migrations applied")
}
