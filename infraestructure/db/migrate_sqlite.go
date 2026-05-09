//go:build sidecar

package db

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
)

// ApplySQLiteMigrations runs every .sql file under `dir` inside `fsys`.
// Like the Postgres runner, files whose numeric prefix is already recorded
// in `_migrations` are skipped — necessary because SQLite's ALTER TABLE has
// no IF NOT EXISTS clause.
//
// `fsys` is an fs.FS so the sidecar can pass an embed.FS (the migrations
// are baked into the binary in production) while tests can pass an
// os.DirFS pointing at the repo's db_migrations/ directory.
func ApplySQLiteMigrations(db *sqlx.DB, fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// path.Join (not filepath.Join): fs.FS uses forward-slash paths
		// regardless of OS. Mixing in filepath.Join breaks on Windows.
		files = append(files, path.Join(dir, e.Name()))
	}
	sort.Strings(files)

	applied, err := loadAppliedSQLiteMigrations(db)
	if err != nil {
		applied = map[int]struct{}{}
	}

	for _, f := range files {
		v, ok := sqliteVersionFromFilename(path.Base(f))
		if ok {
			if _, done := applied[v]; done {
				continue
			}
		}
		raw, err := fs.ReadFile(fsys, f)
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
