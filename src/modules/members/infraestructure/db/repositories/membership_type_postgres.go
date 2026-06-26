//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	"github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type MembershipTypePostgresRepository struct{}

func NewMembershipTypePostgresRepository() *MembershipTypePostgresRepository {
	return &MembershipTypePostgresRepository{}
}

func (r *MembershipTypePostgresRepository) Create(tx sharedDomain.Transaction, mt *mtDomain.MembershipType) (*mtDomain.MembershipType, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := mtToModel(mt)
	if err := gormTx.Create(&m).Error; err != nil {
		return nil, err
	}
	if err := emitMembershipTypeToSync(gormTx, mt); err != nil {
		return nil, err
	}
	return mtFromModel(&m), nil
}

func (r *MembershipTypePostgresRepository) Update(tx sharedDomain.Transaction, mt *mtDomain.MembershipType) (*mtDomain.MembershipType, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	mt.UpdatedAt = time.Now().UTC()
	m := mtToModel(mt)
	if err := gormTx.Where("id = ?", mt.ID).Omit("id", "created_at", "gym_id").Save(&m).Error; err != nil {
		return nil, err
	}
	if err := emitMembershipTypeToSync(gormTx, mt); err != nil {
		return nil, err
	}
	return mtFromModel(&m), nil
}

// emitMembershipTypeToSync escribe la fila correspondiente en
// `sync_entities` para que los sidecars descubran este registro en su
// próximo /sync/pull. Necesario para entidades cuyo origen canónico es
// la nube (membership_types creadas en el wizard de signup desde el
// dashboard) — el push pipeline de sidecar→cloud se encarga del lado
// inverso, pero cloud-direct writes deben emitir manualmente.
//
// Convenciones del payload (cloud→sidecar):
//   - Timestamps en UnixMilli (int64) — el SQLite local los guarda como
//     INTEGER sin cast.
//   - Money fields en CENTS (int64) — el SQLite local tiene `price`,
//     `enrollment_fee` y `maintenance_fee` como INTEGER cents. La
//     dirección sidecar→cloud (enqueueMT en membership_type_sqlite.go)
//     históricamente envía pesos porque el cloud Postgres usa NUMERIC
//     pesos; ese path funciona porque el sidecar nunca re-aplica su
//     propia escritura (LWW lo skipea). Nuestra dirección sí se aplica
//     en el destino, así que escribimos en las unidades del destino.
//   - created_at obligatorio (NOT NULL en SQLite, sin DEFAULT).
//
// Se llama dentro de la misma transacción del Create/Update, así si
// algo falla post-domain-write, el rollback cubre ambos.
func emitMembershipTypeToSync(g *gorm.DB, mt *mtDomain.MembershipType) error {
	// maintenance_frequency: NIL (no fee) o "monthly"/"annual". El
	// CHECK constraint del schema rechaza cualquier otro string,
	// INCLUYENDO "". Si emitimos "" cuando no hay fee, SQLite revienta
	// con CHECK violation. Usamos un puntero para que JSON serialice a
	// `null` en ese caso.
	var freq *string
	if mt.MaintenanceFrequency != nil && *mt.MaintenanceFrequency != "" {
		freq = mt.MaintenanceFrequency
	}
	// El wire de sync lleva el dinero en PESOS (float), igual que products y
	// el resto de tablas: el apply del sidecar (agent_apply.moneyColumns) hace
	// pesos→cents al materializar en SQLite. ANTES acá se mandaba toCents() →
	// el sidecar lo re-multiplicaba ×100 al hacer pull-back, dejando los precios
	// del desktop ×100 (Trimestral $1,300 se veía $130,000). Ahora va en pesos
	// crudos, consistente con product_postgres.go.
	payload, err := json.Marshal(map[string]any{
		"id":                    mt.ID.String(),
		"gym_id":                mt.GymID.String(),
		"version":               mt.Version,
		"name":                  mt.Name,
		"price":                 mt.Price,
		"duration_days":         mt.DurationDays,
		"duration_months":       mt.DurationMonths,
		"enrollment_fee":        mt.EnrollmentFee,
		"maintenance_fee":       mt.MaintenanceFee,
		"maintenance_frequency": freq,
		"active":                mt.Active,
		"created_at":            mt.CreatedAt.UnixMilli(),
		"updated_at":            mt.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	var deletedAt any
	if mt.DeletedAt != nil {
		deletedAt = *mt.DeletedAt
	}
	return g.Exec(`
		INSERT INTO sync_entities
		    (gym_id, entity_type, entity_id, version, payload, server_updated_at, deleted_at)
		VALUES (?, 'membership_types', ?, ?, ?::jsonb, NOW(), ?)
		ON CONFLICT (gym_id, entity_type, entity_id) DO UPDATE SET
			version = EXCLUDED.version,
			payload = EXCLUDED.payload,
			server_updated_at = NOW(),
			deleted_at = EXCLUDED.deleted_at`,
		mt.GymID, mt.ID, mt.Version, string(payload), deletedAt,
	).Error
}

func (r *MembershipTypePostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*mtDomain.MembershipType, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var m models.MembershipTypeModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return mtFromModel(&m), nil
}

func (r *MembershipTypePostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, includeInactive bool) ([]*mtDomain.MembershipType, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	q := gormTx.Where("gym_id = ? AND deleted_at IS NULL", gymID)
	if !includeInactive {
		q = q.Where("active = ?", true)
	}
	var rows []models.MembershipTypeModel
	if err := q.Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*mtDomain.MembershipType, len(rows))
	for i := range rows {
		out[i] = mtFromModel(&rows[i])
	}
	return out, nil
}

func (r *MembershipTypePostgresRepository) ExistsByGymAndName(tx sharedDomain.Transaction, gymID uuid.UUID, name string) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.MembershipTypeModel{}).
		Where("gym_id = ? AND LOWER(name) = ? AND deleted_at IS NULL", gymID, strings.ToLower(name)).
		Count(&n).Error
	return n > 0, err
}

func (r *MembershipTypePostgresRepository) CountActiveMembershipsByType(tx sharedDomain.Transaction, typeID uuid.UUID) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.MembershipModel{}).
		Where("membership_type_id = ? AND status = ? AND deleted_at IS NULL", typeID, "active").
		Count(&n).Error
	return int(n), err
}

func (r *MembershipTypePostgresRepository) CountByGym(tx sharedDomain.Transaction, gymID uuid.UUID) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.MembershipTypeModel{}).
		Where("gym_id = ? AND deleted_at IS NULL", gymID).
		Count(&n).Error
	return int(n), err
}

func mtToModel(mt *mtDomain.MembershipType) models.MembershipTypeModel {
	return models.MembershipTypeModel{
		ID:                   mt.ID,
		GymID:                mt.GymID,
		Version:              mt.Version,
		CreatedAt:            mt.CreatedAt,
		UpdatedAt:            mt.UpdatedAt,
		DeletedAt:            mt.DeletedAt,
		Name:                 mt.Name,
		Price:                mt.Price,
		DurationDays:         mt.DurationDays,
		DurationMonths:       mt.DurationMonths,
		EnrollmentFee:        mt.EnrollmentFee,
		MaintenanceFee:       mt.MaintenanceFee,
		MaintenanceFrequency: mt.MaintenanceFrequency,
		Active:               mt.Active,
	}
}

func mtFromModel(m *models.MembershipTypeModel) *mtDomain.MembershipType {
	return &mtDomain.MembershipType{
		ID:                   m.ID,
		GymID:                m.GymID,
		Version:              m.Version,
		Name:                 m.Name,
		Price:                m.Price,
		DurationDays:         m.DurationDays,
		DurationMonths:       m.DurationMonths,
		EnrollmentFee:        m.EnrollmentFee,
		MaintenanceFee:       m.MaintenanceFee,
		MaintenanceFrequency: m.MaintenanceFrequency,
		Active:               m.Active,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
		DeletedAt:            m.DeletedAt,
	}
}
