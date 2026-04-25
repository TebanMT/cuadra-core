package repository

import (
	"github.com/google/uuid"

	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type MembershipTypeRepository interface {
	Create(tx sharedDomain.Transaction, mt *mtDomain.MembershipType) (*mtDomain.MembershipType, error)
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*mtDomain.MembershipType, error)
	ExistsByGymAndName(tx sharedDomain.Transaction, gymID uuid.UUID, name string) (bool, error)
}
