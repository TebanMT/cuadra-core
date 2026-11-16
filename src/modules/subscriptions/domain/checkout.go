package domain

import "errors"

// Errores del checkout cuando el gym ya tiene un plan que se cobra solo. Un
// checkout nuevo crearía otra suscripción en Stripe y el dueño pagaría dos
// veces. Los mensajes van directo al dueño.
var (
	ErrSubscriptionPastDue = errors.New(
		"Tu último cobro no pasó. Actualiza tu tarjeta en el portal de pagos y se vuelve a intentar; contratar otro plan te cobraría dos veces.")
	ErrSubscriptionRenewsItself = errors.New(
		"Ya tienes un plan que se cobra solo cada periodo. Para cambiar de plan o de tarjeta, usa el portal de pagos.")
	ErrSubscriptionStillRunning = errors.New(
		"Tu plan sigue pagado hasta que termine el periodo. Para conservarlo, reactívalo desde el portal de pagos; contratar otro te cobraría dos veces.")
)

// RenewsAutomatically dice si el pago que dejó el plan vigente viene de una
// suscripción que el procesador cobra sola: tarjeta en Stripe o preapproval
// de Mercado Pago. Un voucher OXXO y un pago manual pagan un periodo y se
// renuevan con un checkout nuevo, así que no cuentan.
func (e *Event) RenewsAutomatically() bool {
	if e == nil || (e.Type != EventActivated && e.Type != EventRenewed) {
		return false
	}
	switch e.Provider {
	case ProviderStripe:
		method, _ := e.RawPayload["payment_method"].(string)
		return method != "oxxo"
	case ProviderMercadoPago:
		return true
	}
	return false
}
