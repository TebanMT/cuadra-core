//go:build server

// Package tenantbackup provides a tenant-scoped, authenticated encrypted backup.
// Credentials and biometric ciphertext remain inside the encrypted archive.
package tenantbackup

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type table struct {
	Name string          `json:"name"`
	Rows json.RawMessage `json:"rows"`
}
type archive struct {
	Format    int       `json:"format"`
	Gym       uuid.UUID `json:"gym_id"`
	Created   time.Time `json:"created_at"`
	Migration int       `json:"migration"`
	Tables    []table   `json:"tables"`
}
type Summary struct {
	GymID     uuid.UUID `json:"gym_id"`
	Tables    int       `json:"tables"`
	Rows      int       `json:"rows"`
	Migration int       `json:"migration"`
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func tenantTables(tx *gorm.DB) ([]string, error) {
	var names []string
	err := tx.Raw(`SELECT c.table_name FROM information_schema.columns c JOIN information_schema.tables t USING(table_schema,table_name) WHERE c.table_schema='public' AND c.column_name='gym_id' AND t.table_type='BASE TABLE' ORDER BY c.table_name`).Scan(&names).Error
	return names, err
}

// Export reads all tenant tables from one PostgreSQL repeatable-read snapshot.
func Export(ctx context.Context, db *gorm.DB, gym uuid.UUID, key []byte) ([]byte, Summary, error) {
	a := archive{Format: 1, Gym: gym, Created: time.Now().UTC()}
	if gym == uuid.Nil {
		return nil, Summary{}, errors.New("gym_id required")
	}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT set_config('tinta.gym_id',?,true)`, gym.String()).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Raw(`SELECT count(*) FROM gyms WHERE id=? AND gym_id=?`, gym, gym).Scan(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return errors.New("gym not found")
		}
		if err := tx.Raw(`SELECT COALESCE(MAX(version),0) FROM _migrations`).Scan(&a.Migration).Error; err != nil {
			return err
		}
		names, err := tenantTables(tx)
		if err != nil {
			return err
		}
		for _, name := range names {
			var raw string
			if err := tx.Raw(`SELECT COALESCE(jsonb_agg(to_jsonb(t)),'[]'::jsonb)::text FROM `+quote(name)+` t WHERE gym_id=?`, gym).Scan(&raw).Error; err != nil {
				return err
			}
			a.Tables = append(a.Tables, table{Name: name, Rows: json.RawMessage(raw)})
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, Summary{}, err
	}
	data, err := json.Marshal(a)
	if err != nil {
		return nil, Summary{}, err
	}
	encrypted, err := seal(data, key)
	return encrypted, summarize(a), err
}

func summarize(a archive) Summary {
	s := Summary{GymID: a.Gym, Tables: len(a.Tables), Migration: a.Migration}
	for _, t := range a.Tables {
		var rows []json.RawMessage
		_ = json.Unmarshal(t.Rows, &rows)
		s.Rows += len(rows)
	}
	return s
}

func seal(data, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("backup key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(append([]byte("TINTA1"), nonce...), nonce, data, []byte("TINTA1")), nil
}
func open(data, key []byte) (archive, error) {
	var a archive
	if len(key) != 32 {
		return a, errors.New("backup key must contain exactly 32 bytes")
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(data) < 6+gcm.NonceSize() || string(data[:6]) != "TINTA1" {
		return a, errors.New("invalid backup")
	}
	raw, err := gcm.Open(nil, data[6:6+gcm.NonceSize()], data[6+gcm.NonceSize():], data[:6])
	if err != nil {
		return a, errors.New("backup authentication failed")
	}
	if err = json.Unmarshal(raw, &a); err != nil {
		return a, errors.New("invalid backup document")
	}
	if a.Format != 1 || a.Gym == uuid.Nil {
		return a, errors.New("unsupported backup format")
	}
	return a, nil
}

// Restore is maintenance-only. The caller must restrict the target to an empty,
// isolated local database. Table ownership is required to suspend USER triggers;
// foreign keys remain active, including tenant reference constraints.
func Restore(ctx context.Context, db *gorm.DB, encrypted, key []byte) (Summary, error) {
	a, err := open(encrypted, key)
	if err != nil {
		return Summary{}, err
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var migration int
		if err := tx.Raw(`SELECT COALESCE(MAX(version),0) FROM _migrations`).Scan(&migration).Error; err != nil {
			return err
		}
		if migration != a.Migration {
			return fmt.Errorf("migration mismatch: archive %d, target %d", a.Migration, migration)
		}
		names, err := tenantTables(tx)
		if err != nil {
			return err
		}
		allowed := map[string]bool{}
		for _, n := range names {
			allowed[n] = true
		}
		content := map[string]json.RawMessage{}
		for _, t := range a.Tables {
			if !allowed[t.Name] || content[t.Name] != nil {
				return errors.New("unexpected or duplicated table")
			}
			var rows []map[string]any
			if err := json.Unmarshal(t.Rows, &rows); err != nil {
				return err
			}
			for _, r := range rows {
				if r["gym_id"] != a.Gym.String() {
					return errors.New("cross-tenant archive row")
				}
			}
			content[t.Name] = t.Rows
		}
		if len(content) != len(names) {
			return errors.New("incomplete table inventory")
		}
		// Refuse a target with *any* tenant data. This is a recovery drill, never an overwrite.
		for _, name := range names {
			var count int64
			if err := tx.Raw(`SELECT count(*) FROM ` + quote(name)).Scan(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return errors.New("restore target must have no tenant data")
			}
			if err := tx.Exec(`ALTER TABLE ` + quote(name) + ` DISABLE TRIGGER USER`).Error; err != nil {
				return err
			}
		}

		// Cyclic graphs (expense↔cash, payment↔membership) must be checked
		// after all rows exist. Keep FK triggers enabled, temporarily defer
		// their checks inside this maintenance transaction, then restore the
		// exact schema flags after successful validation.
		var constraints []struct {
			Table      string
			Name       string
			Deferrable bool
			Deferred   bool
		}
		if err := tx.Raw(`SELECT child.relname AS table,c.conname AS name,c.condeferrable AS deferrable,c.condeferred AS deferred FROM pg_constraint c JOIN pg_class child ON child.oid=c.conrelid JOIN pg_namespace n ON n.oid=child.relnamespace WHERE c.contype='f' AND n.nspname='public'`).Scan(&constraints).Error; err != nil {
			return err
		}
		for _, c := range constraints {
			if allowed[c.Table] {
				if err := tx.Exec(`ALTER TABLE ` + quote(c.Table) + ` ALTER CONSTRAINT ` + quote(c.Name) + ` DEFERRABLE INITIALLY DEFERRED`).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Exec(`SET CONSTRAINTS ALL DEFERRED`).Error; err != nil {
			return err
		}
		for _, name := range names {
			var columns []string
			if err := tx.Raw(`SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name=? AND is_generated='NEVER' ORDER BY ordinal_position`, name).Scan(&columns).Error; err != nil {
				return err
			}
			for i, c := range columns {
				columns[i] = quote(c)
			}
			selection := strings.Join(columns, ",")
			if err := tx.Exec(`INSERT INTO `+quote(name)+` (`+selection+`) SELECT `+selection+` FROM jsonb_populate_recordset(NULL::`+quote(name)+`,?::jsonb)`, string(content[name])).Error; err != nil {
				return fmt.Errorf("restore table %s: %w", name, err)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			var sequences []struct {
				Column   string
				Sequence string
			}
			if err := tx.Raw(`SELECT column_name AS column,pg_get_serial_sequence(quote_ident(table_name),column_name) AS sequence FROM information_schema.columns WHERE table_schema='public' AND table_name=? AND (column_default LIKE 'nextval(%' OR is_identity='YES')`, name).Scan(&sequences).Error; err != nil {
				return err
			}
			for _, seq := range sequences {
				if seq.Sequence != "" {
					if err := tx.Exec(`SELECT setval(?::regclass,GREATEST(COALESCE(MAX(`+quote(seq.Column)+`),1),1),COUNT(*)>0) FROM `+quote(name), seq.Sequence).Error; err != nil {
						return err
					}
				}
			}
		}
		if err := tx.Exec(`SET CONSTRAINTS ALL IMMEDIATE`).Error; err != nil {
			return err
		}
		for _, name := range names {
			if err := tx.Exec(`ALTER TABLE ` + quote(name) + ` ENABLE TRIGGER USER`).Error; err != nil {
				return err
			}
		}
		for _, c := range constraints {
			if !allowed[c.Table] {
				continue
			}
			flags := "NOT DEFERRABLE"
			if c.Deferrable {
				flags = "DEFERRABLE INITIALLY IMMEDIATE"
				if c.Deferred {
					flags = "DEFERRABLE INITIALLY DEFERRED"
				}
			}
			if err := tx.Exec(`ALTER TABLE ` + quote(c.Table) + ` ALTER CONSTRAINT ` + quote(c.Name) + ` ` + flags).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return summarize(a), err
}

// Verify compares every restored row, including money, dates and tombstones,
// against the authenticated archive without printing its contents.
func Verify(ctx context.Context, db *gorm.DB, encrypted, key []byte) (Summary, error) {
	a, err := open(encrypted, key)
	if err != nil {
		return Summary{}, err
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT set_config('tinta.gym_id',?,true)`, a.Gym.String()).Error; err != nil {
			return err
		}
		names, err := tenantTables(tx)
		if err != nil {
			return err
		}
		allowed := map[string]bool{}
		for _, name := range names {
			allowed[name] = true
		}
		for _, t := range a.Tables {
			if !allowed[t.Name] {
				return errors.New("unknown archive table")
			}
			q := quote(t.Name)
			var differences int64
			query := `WITH actual AS (SELECT to_jsonb(t) AS row FROM ` + q + ` t WHERE gym_id=?), expected AS (SELECT to_jsonb(t) AS row FROM jsonb_populate_recordset(NULL::` + q + `,?::jsonb) t) SELECT count(*) FROM ((SELECT row FROM actual EXCEPT ALL SELECT row FROM expected) UNION ALL (SELECT row FROM expected EXCEPT ALL SELECT row FROM actual)) differences`
			if err := tx.Raw(query, a.Gym, string(t.Rows)).Scan(&differences).Error; err != nil {
				return err
			}
			if differences != 0 {
				return fmt.Errorf("verification differs in table %s", t.Name)
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return summarize(a), err
}
