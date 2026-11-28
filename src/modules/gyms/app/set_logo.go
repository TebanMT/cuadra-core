package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/logocache"
)

// LogoPublisher guarda el PNG ya preparado en almacenamiento público y
// devuelve su URL absoluta. cmd/server lo cablea al bucket público de R2
// (el mismo de los banners de bienvenida); sin bucket, queda nil y la
// subida responde «no disponible».
type LogoPublisher interface {
	PublishLogo(ctx context.Context, gymID uuid.UUID, png []byte) (publicURL string, err error)
}

// GymSyncToucher re-manda la fila del gym a los desktops (sync_entities).
// Lo satisface sync.PostgresStore.TouchGym.
type GymSyncToucher interface {
	TouchGym(ctx context.Context, tx sharedDomain.Transaction, gymID uuid.UUID) error
}

// SetGymLogo — el dueño sube o quita el logo del gym (ticket formal y
// dashboard) desde un botón, sin pegar URLs.
//
// Es cloud-authoritative, igual que la conexión de WhatsApp: los bytes viven
// en el bucket público y la URL vuelve a cada desktop por el pull (TouchGym
// aquí + logo_url inyectado por canonicalAugmentExpr). El sidecar no corre
// este caso de uso: GymLogoSidecarProxy lo reenvía al cloud con el sk_live_*
// del pareo. Un logo en disco local (el diseño anterior) sólo existía en la
// máquina que lo subió.
type SetGymLogo struct {
	Gyms      gymRepo.GymRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	Publisher LogoPublisher
	// SyncTouch es opcional para pruebas; cmd/server lo cablea después de
	// construir el sync store (mismo patrón que ConnectWhatsApp).
	SyncTouch GymSyncToucher
}

func NewSetGymLogo(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder, publisher LogoPublisher) *SetGymLogo {
	return &SetGymLogo{Gyms: gyms, UoW: uow, Audit: recorder, Publisher: publisher}
}

// CanUpload — false cuando el binario no tiene dónde publicar el logo.
func (uc *SetGymLogo) CanUpload() bool {
	return uc != nil && uc.Publisher != nil
}

type UploadGymLogoInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	// Image son los bytes tal como los subió el dueño (PNG/JPEG/GIF/WebP).
	Image []byte
}

// Upload prepara la imagen (PNG limpio, máximo 1024 px), la publica y deja
// su URL en el gym.
func (uc *SetGymLogo) Upload(ctx context.Context, in UploadGymLogoInput) (*gymDomain.Gym, error) {
	png, err := logocache.PrepareUpload(in.Image)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	url, err := uc.Publisher.PublishLogo(ctx, in.GymID, png)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return uc.set(ctx, in.GymID, in.ActorUserID, url)
}

// Remove quita el logo: el ticket y el dashboard salen sin él. El objeto
// publicado no se borra — su llave es por contenido y otro gym nunca la
// reutiliza; volver a subir el mismo archivo recupera la misma URL.
func (uc *SetGymLogo) Remove(ctx context.Context, gymID, actorUserID uuid.UUID) (*gymDomain.Gym, error) {
	return uc.set(ctx, gymID, actorUserID, "")
}

func (uc *SetGymLogo) set(ctx context.Context, gymID, actorUserID uuid.UUID, url string) (*gymDomain.Gym, error) {
	var out *gymDomain.Gym
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		g, err := uc.Gyms.GetByID(tx, gymID)
		if err != nil {
			return err
		}
		if err := g.ApplyProfileUpdate(gymDomain.ProfileUpdate{LogoURL: &url}); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Gyms.Update(tx, g)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var logged any
		if url != "" {
			logged = url
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       gymID,
			EntityType:  "gyms",
			EntityID:    gymID,
			Action:      audit.ActionUpdate,
			ActorUserID: &actorUserID,
			Changes:     map[string]any{"logo_url": logged},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          time.Now().UTC(),
		})
		if uc.SyncTouch != nil {
			if err := uc.SyncTouch.TouchGym(ctx, tx, gymID); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		out = updated
		return nil
	})
	return out, err
}
