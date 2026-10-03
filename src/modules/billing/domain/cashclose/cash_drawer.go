package cashclose

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	DefaultDrawerName  = "Caja principal"
	maxDrawerNameRunes = 60
)

var (
	ErrDrawerNotFound            = errors.New("la caja no existe")
	ErrDrawerNameRequired        = errors.New("indica un nombre para la caja")
	ErrDrawerNameTooLong         = errors.New("el nombre de la caja no puede exceder 60 caracteres")
	ErrDrawerNameConflict        = errors.New("ya existe una caja con ese nombre")
	ErrDrawerIdempotencyRequired = errors.New("indica una clave de idempotencia")
	ErrDrawerIdempotencyConflict = errors.New("la clave de idempotencia ya fue usada con datos distintos")
	ErrDrawerVersionConflict     = errors.New("la caja cambió en otro dispositivo; actualiza e intenta de nuevo")
	ErrDrawerOwnerRequired       = errors.New("sólo el propietario puede administrar cajas")
	ErrMainDrawerDeactivate      = errors.New("la caja principal no se puede desactivar")
	ErrDrawerInactive            = errors.New("la caja está desactivada")
	ErrDrawerHasOpenActivity     = errors.New("la caja tiene efectivo pendiente de conciliar o retirar")
)

var drawerNamespace = uuid.MustParse("c4e73391-02ff-57c5-b090-89bb922fd971")

// CashDrawer is the stable, human-readable catalog entry for a physical cash
// location. Physical ledgers keep drawer_id snapshots, while this aggregate
// owns only configuration; deactivation never erases historical cash events.
type CashDrawer struct {
	ID             uuid.UUID
	GymID          uuid.UUID
	Version        int
	Code           string
	Name           string
	Active         bool
	IsMain         bool
	IdempotencyKey *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time
}

func DeterministicDrawerID(gymID uuid.UUID, idempotencyKey string) uuid.UUID {
	return uuid.NewSHA1(drawerNamespace, []byte(gymID.String()+"|"+strings.TrimSpace(idempotencyKey)))
}

func NewMainDrawer(gymID uuid.UUID, now time.Time) *CashDrawer {
	now = now.UTC()
	return &CashDrawer{ID: DefaultDrawerID(gymID), GymID: gymID, Version: 1,
		Code: DefaultDrawerCode, Name: DefaultDrawerName, Active: true, IsMain: true,
		CreatedAt: now, UpdatedAt: now}
}

func NewDrawer(gymID uuid.UUID, name, idempotencyKey string, now time.Time) (*CashDrawer, error) {
	name, err := cleanDrawerName(name)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return nil, ErrDrawerIdempotencyRequired
	}
	id := DeterministicDrawerID(gymID, key)
	now = now.UTC()
	return &CashDrawer{ID: id, GymID: gymID, Version: 1,
		Code: DrawerCode(gymID, id), Name: name, Active: true, IsMain: false,
		IdempotencyKey: &key, CreatedAt: now, UpdatedAt: now}, nil
}

func (d *CashDrawer) Rename(name string, now time.Time) error {
	cleaned, err := cleanDrawerName(name)
	if err != nil {
		return err
	}
	if d.Name == cleaned {
		return nil
	}
	d.Name = cleaned
	d.bumpDrawer(now)
	return nil
}

func (d *CashDrawer) SetActive(active bool, now time.Time) error {
	if d.IsMain && !active {
		return ErrMainDrawerDeactivate
	}
	if d.Active == active {
		return nil
	}
	d.Active = active
	d.bumpDrawer(now)
	return nil
}

func (d *CashDrawer) bumpDrawer(now time.Time) {
	d.Version++
	d.UpdatedAt = now.UTC()
}

func cleanDrawerName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrDrawerNameRequired
	}
	if utf8.RuneCountInString(name) > maxDrawerNameRunes {
		return "", ErrDrawerNameTooLong
	}
	return name, nil
}
