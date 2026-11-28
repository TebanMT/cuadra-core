//go:build server

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notiBanner "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/banner"
	"github.com/cuadra/cuadra-core/src/shared/r2"
)

// welcomeBannerRenderer adapts the pure banner.Render function to the
// notifications app interface (ADR-009 media welcome templates).
type welcomeBannerRenderer struct{}

func (welcomeBannerRenderer) RenderWelcome(_ context.Context, in notiApp.WelcomeBannerInput) ([]byte, string, error) {
	kind := notiBanner.KindMember
	switch {
	case in.Owner:
		kind = notiBanner.KindOwner
	case in.Operator:
		kind = notiBanner.KindOperator
	}
	return notiBanner.Render(notiBanner.Input{
		Kind:    kind,
		Name:    in.Name,
		GymName: in.GymName,
		PIN:     in.PIN,
	})
}

// r2PublicUploader adapts the R2 client's public PutObject to the
// notifications app PublicMediaUploader interface.
type r2PublicUploader struct{ c *r2.Client }

func (u r2PublicUploader) UploadPublic(ctx context.Context, objectKey, contentType string, body []byte) (string, error) {
	return u.c.PutPublicObject(ctx, objectKey, contentType, body)
}

// r2LogoPublisher adapta el bucket público al puerto gymApp.LogoPublisher.
// La llave es por contenido (gym + sha256 del PNG ya preparado): subir el
// mismo archivo dos veces no duplica objetos, y una URL nueva por cada logo
// distinto invalida sola las copias cacheadas (navegador, CDN, logocache).
type r2LogoPublisher struct{ c *r2.Client }

func (p r2LogoPublisher) PublishLogo(ctx context.Context, gymID uuid.UUID, png []byte) (string, error) {
	sum := sha256.Sum256(png)
	key := fmt.Sprintf("gym-logos/%s/%s.png", gymID, hex.EncodeToString(sum[:12]))
	return p.c.PutPublicObject(ctx, key, "image/png", png)
}

// receiptPDFRenderer adapta billing.GenerateReceipt a la interfaz
// notiApp.ReceiptPDFRenderer: el dispatcher genera el PDF del comprobante
// (el cloud tiene el pago + gym + socio sincronizados), lo sube a R2 público
// y mete la URL como {receipt_url} del template de recibo.
type receiptPDFRenderer struct{ gen *billingApp.GenerateReceipt }

func (r receiptPDFRenderer) RenderReceiptPDF(ctx context.Context, gymID, paymentID uuid.UUID) ([]byte, string, error) {
	out, err := r.gen.Execute(ctx, billingApp.GenerateReceiptInput{GymID: gymID, PaymentID: paymentID})
	if err != nil {
		return nil, "", err
	}
	return out.PDF, out.ContentType, nil
}
