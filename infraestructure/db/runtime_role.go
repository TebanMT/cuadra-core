//go:build server

package db

import (
	"errors"
	"gorm.io/gorm"
	"strings"
)

// GrantRuntimeRole is run only by the migrator. Runtime receives DML, never
// ownership, CREATE, TRUNCATE, membership in the owner role or RLS bypass.
func GrantRuntimeRole(db *gorm.DB, role string) error {
	var eligible bool
	if err := db.Raw(`SELECT EXISTS(SELECT 1 FROM pg_roles r WHERE r.rolname=? AND NOT r.rolsuper AND NOT r.rolbypassrls AND NOT r.rolcreaterole AND NOT r.rolcreatedb AND r.rolname<>current_user AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='r' AND pg_has_role(r.oid,c.relowner,'MEMBER')))`, role).Scan(&eligible).Error; err != nil {
		return err
	}
	if !eligible {
		return errors.New("runtime role must exist, own no tables and have no privileged role membership")
	}
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	grantee := quote(role)
	return db.Transaction(func(tx *gorm.DB) error {
		for _, statement := range []string{
			`REVOKE CREATE ON SCHEMA public FROM PUBLIC`,
			`REVOKE CREATE ON SCHEMA public,tinta_private FROM ` + grantee,
			`REVOKE TRUNCATE,REFERENCES,TRIGGER ON ALL TABLES IN SCHEMA public FROM ` + grantee,
			`GRANT USAGE ON SCHEMA public,tinta_private TO ` + grantee,
			`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA tinta_private TO ` + grantee,
			`GRANT SELECT ON ALL TABLES IN SCHEMA public TO ` + grantee,
			`GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + grantee,
		} {
			if err := tx.Exec(statement).Error; err != nil {
				return err
			}
		}
		var tables []string
		if err := tx.Raw(`SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename<>'_migrations'`).Scan(&tables).Error; err != nil {
			return err
		}
		for _, table := range tables {
			if err := tx.Exec(`GRANT INSERT,UPDATE,DELETE ON TABLE public.` + quote(table) + ` TO ` + grantee).Error; err != nil {
				return err
			}
		}
		return tx.Exec(`REVOKE INSERT,UPDATE,DELETE,TRUNCATE ON TABLE public._migrations FROM ` + grantee).Error
	})
}
