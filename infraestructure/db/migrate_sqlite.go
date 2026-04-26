//go:build sidecar

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
)

// ApplySQLiteMigrations runs every .sql file under db_migrations/sqlite/.
// Like the Postgres runner, files whose numeric prefix is already recorded
// in `_migrations` are skipped — necessary because SQLite's ALTER TABLE has
// no IF NOT EXISTS clause.
func ApplySQLiteMigrations(db *sqlx.DB, dir string) error {
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

	applied, err := loadAppliedSQLiteMigrations(db)
	if err != nil {
		applied = map[int]struct{}{}
	}

	for _, f := range files {
		v, ok := sqliteVersionFromFilename(filepath.Base(f))
		if ok {
			if _, done := applied[v]; done {
				continue
			}
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %q: %w", f, err)
		}
		if _, err := db.Exec(string(raw)); err != nil {
			return fmt.Errorf("apply %q: %w", f, err)
		}
	}
	return nil
}

func loadAppliedSQLiteMigrations(db *sqlx.DB) (map[int]struct{}, error) {
	var versions []int
	if err := db.Select(&versions, "SELECT version FROM _migrations"); err != nil {
		return nil, err
	}
	out := make(map[int]struct{}, len(versions))
	for _, v := range versions {
		out[v] = struct{}{}
	}
	return out, nil
}

func sqliteVersionFromFilename(name string) (int, bool) {
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
