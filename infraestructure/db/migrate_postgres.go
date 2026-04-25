//go:build server

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// ApplyPostgresMigrations runs every .sql file under db_migrations/postgres/
// in lexicographic order. Each file is its own transaction — the SQL itself
// uses BEGIN/COMMIT so wrapping is unnecessary. Idempotent: every file uses
// IF NOT EXISTS.
func ApplyPostgresMigrations(db *gorm.DB, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %q: %w", f, err)
		}
		if err := db.Exec(string(raw)).Error; err != nil {
			return fmt.Errorf("apply %q: %w", f, err)
		}
	}
	return nil
}
