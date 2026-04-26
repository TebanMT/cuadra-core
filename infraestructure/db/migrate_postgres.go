//go:build server

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

// ApplyPostgresMigrations runs every .sql file under db_migrations/postgres/
// in lexicographic order. Each file is its own transaction — the SQL itself
// uses BEGIN/COMMIT so wrapping is unnecessary. The runner consults the
// `_migrations` table and skips files whose version is already recorded; that
// covers the cases where individual statements (e.g. ALTER TABLE) are not
// natively idempotent. Files with a non-numeric prefix are always run.
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

	applied, err := loadAppliedPostgresMigrations(db)
	if err != nil {
		// _migrations may not exist on first boot — that's fine, every file runs.
		applied = map[int]struct{}{}
	}

	for _, f := range files {
		v, ok := versionFromFilename(filepath.Base(f))
		if ok {
			if _, done := applied[v]; done {
				continue
			}
		}
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

func loadAppliedPostgresMigrations(db *gorm.DB) (map[int]struct{}, error) {
	var rows []struct {
		Version int
	}
	if err := db.Raw("SELECT version FROM _migrations").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[int]struct{}, len(rows))
	for _, r := range rows {
		out[r.Version] = struct{}{}
	}
	return out, nil
}

// versionFromFilename parses the leading integer of a migration filename:
//
//	"001_init_schema.sql" -> 1, true
//	"002_notifications.sql" -> 2, true
//	"adhoc.sql"           -> 0, false
func versionFromFilename(name string) (int, bool) {
	cut := strings.IndexByte(name, '_')
	if cut <= 0 {
		return 0, false
	}
	v, err := strconv.Atoi(name[:cut])
	if err != nil {
		return 0, false
	}
	return v, true
}
