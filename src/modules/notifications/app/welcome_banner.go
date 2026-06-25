package app

import (
	"context"

	"github.com/google/uuid"
)

// ADR-009: los templates de bienvenida (socio/operador) son Media en Twilio.
// El PIN no puede ir como texto — Meta no lo aprueba — así que se hornea en
// un banner que el dispatcher genera al vuelo, lo sube a un bucket público y
// pasa la URL como variable de media ({{1}}) del template.
//
// Estas interfaces viven en app; las implementan piezas de infraestructure
// (renderer de imagen + uploader R2), inyectadas en cmd/server/main.go. Son
// server-only de facto: el sidecar nunca despacha WhatsApp.

// WelcomeBannerInput es la data que se hornea en el banner.
type WelcomeBannerInput struct {
	Operator bool // socio=false/operador=true (ignorado si Owner)
	Owner    bool // true = banner "tu sistema ya está vivo" del dueño
	Name     string
	GymName  string
	PIN      string
}

// WelcomeBannerRenderer renderiza el PNG del banner de bienvenida.
type WelcomeBannerRenderer interface {
	RenderWelcome(ctx context.Context, in WelcomeBannerInput) (body []byte, contentType string, err error)
}

// PublicMediaUploader guarda bytes en storage público y devuelve una URL
// descargable sin auth (Twilio/Meta bajan el banner anónimamente).
type PublicMediaUploader interface {
	UploadPublic(ctx context.Context, objectKey, contentType string, body []byte) (publicURL string, err error)
}

// ReceiptPDFRenderer genera el PDF del comprobante de un pago. Lo implementa el
// binario server envolviendo billing.GenerateReceipt (que tiene el payment +
// gym + socio ya sincronizados); el dispatcher lo sube a R2 público vía
// PublicMediaUploader y mete la URL como {receipt_url} del template de recibo.
// Server-only de facto: el sidecar no despacha WhatsApp.
type ReceiptPDFRenderer interface {
	RenderReceiptPDF(ctx context.Context, gymID, paymentID uuid.UUID) (body []byte, contentType string, err error)
}

// receiptTemplates — templates de recibo que llevan la URL del comprobante
// (PDF en R2) como variable. Las keys coinciden con template.DefaultLibrary y
// con receiptTemplateForConcept (enqueue_receipt.go).
var receiptTemplates = map[string]bool{
	"receipt_membership": true,
	"receipt_product":    true,
}

// bannerTemplateSpec describe cómo un template Media de bienvenida se mapea a
// un render + a las content variables de Twilio.
type bannerTemplateSpec struct {
	operator bool
	owner    bool
	nameKey  string // clave del payload con el nombre del destinatario
}

// bannerTemplates es el conjunto cerrado de templates Media que requieren un
// banner generado. Las keys deben coincidir con las de template.DefaultLibrary.
var bannerTemplates = map[string]bannerTemplateSpec{
	"member_welcome_number": {operator: false, nameKey: "member_first_name"},
	"operator_welcome_pin":  {operator: true, nameKey: "full_name"},
	"owner_welcome":         {owner: true, nameKey: "full_name"},
}

// buildVars arma las content variables del template Media. El ORDEN posicional
// lo fija template.DefaultLibrary (contentVariablesJSON mapea por nombre→pos);
// aquí sólo producimos el conjunto correcto de claves:
//   - member_welcome_number: {member_first_name, gym_name, welcome_image_url} ({{1}},{{2}},{{3}})
//   - operator_welcome_pin: {welcome_image_url, full_name, gym_name}          ({{1}},{{2}},{{3}})
func (s bannerTemplateSpec) buildVars(payload map[string]string, imageURL string) map[string]string {
	out := map[string]string{
		"welcome_image_url": imageURL,
		"gym_name":          payload["gym_name"],
	}
	if s.operator || s.owner {
		out["full_name"] = payload["full_name"]
	} else {
		out["member_first_name"] = payload["member_first_name"]
	}
	return out
}
