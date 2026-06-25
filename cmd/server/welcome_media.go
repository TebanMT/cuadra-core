//go:build server

package main

import (
	"context"

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
