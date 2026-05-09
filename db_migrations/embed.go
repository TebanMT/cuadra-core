// Package migrations embeds the SQLite migration files into the binary so
// that the sidecar can run from any working directory (including inside a
// macOS .app bundle, where cwd is "/" and relative paths break).
//
// Postgres migrations are NOT embedded — the cloud server runs from a
// known deploy path with the migrations directory next to the binary,
// and we want to be able to add/edit migrations without rebuilding the
// server image. The sidecar's deployment story is different: it ships
// inside Tauri bundles and must be 100% self-contained.
package migrations

import "embed"

// SQLite contains every .sql file under db_migrations/sqlite/.
// Use fs.Sub or pass the subdir name to ApplySQLiteMigrations.
//
//go:embed sqlite/*.sql
var SQLite embed.FS
