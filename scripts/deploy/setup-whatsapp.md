# WhatsApp setup para Tinta (envío vía Twilio)

Tinta usa Twilio para WhatsApp porque:

1. Twilio tiene WhatsApp Business API oficial (no es scraper, no se rompe)
2. Soporta plantillas pre-aprobadas (HSM templates) que es lo que Meta exige para mensajes proactivos
3. SDK Go robusto, ya integrado en `cuadra-core/src/modules/notifications/infraestructure/whatsapp/twilio.go`
4. Costos predecibles: ~$0.005 USD por mensaje en MX
5. Twilio te puede asignar un número y maneja el flow de aprobación con Meta

Alternativas como WhatsApp Business directo (Cloud API de Meta) son más baratas pero MUCHO más fricción de setup. Twilio vale la pena.

Tiempo total: depende de Meta (1-3 días de revisión cuando registras el número/business). Una vez aprobado, configurar las env vars en Tinta toma ~5 min.

## Pre-requisitos (lo que estás haciendo ahora)

1. **Cuenta WhatsApp Business** vinculada a un número que NO uses para WhatsApp personal. Idealmente un chip nuevo.
2. **Verificación del Meta Business Manager** — Meta exige que tu negocio esté verificado para mandar mensajes "outbound" iniciados (templates HSM). Verificación = subir docs (acta constitutiva, RFC, identificación, etc.) y esperar revisión.
3. **Cuenta Twilio** activada con métodos de pago.

Si no terminaste alguno de los 3 todavía, **termina esos primero**. Lo que sigue asume que ya tienes el número WhatsApp Business operativo y la cuenta Twilio creada.

## Paso 1 — Conectar WhatsApp con Twilio

```
Twilio Console → Messaging → Senders → WhatsApp senders
   → Add Sender
      Provider: Twilio (no third-party)
      WhatsApp Business Account: <tu Meta Business>
      Phone Number: <tu número WhatsApp Business>
      → Continue
```

Twilio te pide:

- Display name (lo que ven los clientes en WA): "Tinta"
- Categoría del business: "Software / SaaS"
- Descripción corta
- URL: `https://entinta.mx`
- Email de contacto: `soporte@entinta.mx`

Después de submit, Meta lo revisa. **Tarda 1-3 días hábiles.**

Mientras esperas aprobación, puedes seguir desarrollando con el Twilio Sandbox (un número compartido `+1 415 523 8886` con prefijo). Útil para testing pero no para producción.

## Paso 2 — Crear plantillas (HSM templates) en Meta

WhatsApp Business **prohíbe** mandar mensajes outbound iniciados con texto libre. Tenés que usar **plantillas pre-aprobadas** por Meta. Tinta necesita estas **18 plantillas**:

### Cómo se crean

Para cada plantilla de abajo:

```
Twilio Console → Messaging → Content Template Builder
   → Create new template
      Friendly Name: <key de la plantilla>
      Category: <categoría indicada>
      Language: es_MX (Spanish - Mexico)
      Body: <cuerpo con {{1}}, {{2}}...>
      → Submit for approval
```

Meta aprueba en **1–24h** (utility suele ser rápido, marketing más lento, authentication casi inmediato).

Una vez aprobada cada plantilla, Twilio te da un **Content SID** (`HXxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx`).
Guárdalos — los necesitas en el Paso 4 para configurar las env vars.

---

### UTILITY (15 plantillas)

#### `expiry_reminder_3d`
```
Hola {{1}} 👋 Soy {{2}}. Te aviso que tu mensualidad vence el {{3}}. ¿Te esperamos para renovar?
```
`{{1}}`=member_first_name · `{{2}}`=gym_name · `{{3}}`=expiry_date

---

#### `expiry_reminder_today`
```
Hola {{1}}, hoy vence tu mensualidad en {{2}}. ¡Pásate cuando puedas!
```
`{{1}}`=member_first_name · `{{2}}`=gym_name

---

#### `expiry_reminder_5d_post`
```
Hola {{1}}, te extrañamos en {{2}}. Te dejamos esta nota por si quieres regresar.
```
`{{1}}`=member_first_name · `{{2}}`=gym_name

---

#### `receipt_membership`
```
¡Listo, {{1}}! Recibimos tu pago de ${{2}} por {{3}}. Tu nueva vigencia es hasta el {{4}}. Descarga tu comprobante: {{5}} — {{6}}.
```
`{{1}}`=member_first_name · `{{2}}`=amount · `{{3}}`=membership_type · `{{4}}`=expiry_date · `{{5}}`=receipt_url · `{{6}}`=gym_name

---

#### `receipt_product`
```
¡Listo, {{1}}! Tu compra de ${{2}} en {{3}} fue registrada. Tu comprobante: {{4}}
```
`{{1}}`=member_first_name · `{{2}}`=amount · `{{3}}`=gym_name · `{{4}}`=receipt_url

---

#### `owner_alert_low_stock`
```
Alerta de {{1}}: stock bajo de {{2}} ({{3}} unidades).
```
`{{1}}`=gym_name · `{{2}}`=product_name · `{{3}}`=stock

---

#### `owner_alert_expired_batch`
```
Alerta de {{1}}: {{2}} socios vencidos sin contacto. Persíguelos en Cuadra.
```
`{{1}}`=gym_name · `{{2}}`=count

---

#### `owner_alert_cash_close_diff`
```
Alerta de {{1}}: cierre de caja con diferencia de ${{2}}. Revisa en Cuadra.
```
`{{1}}`=gym_name · `{{2}}`=diff_amount

---

#### `owner_alert_vip_no_visit`
```
Alerta de {{1}}: {{2}} (socio VIP) no viene desde hace {{3}} días.
```
`{{1}}`=gym_name · `{{2}}`=member_name · `{{3}}`=days_inactive

---

#### `owner_alert_no_payments_today`
```
Alerta de {{1}}: no se registraron cobros el {{2}}. Revisa con tu operador.
```
`{{1}}`=gym_name · `{{2}}`=date

---

#### `member_welcome_pin`
```
Hola {{1}} 👋 Soy {{2}}. Tu PIN de acceso es *{{3}}*. Lo usas en el kiosko del gym para registrar tu entrada. ¡Bienvenido!
```
`{{1}}`=member_first_name · `{{2}}`=gym_name · `{{3}}`=pin

---

#### `oxxo_renewal_reminder_30d`
```
Hola {{1}} 👋 Tu plan anual de Tinta vence el {{2}}. Te dejamos tu link para pagar la próxima ficha en OXXO cuando puedas: {{3}}
```
`{{1}}`=gym_name · `{{2}}`=expires_on · `{{3}}`=voucher_url

---

#### `oxxo_renewal_reminder_14d`
```
Hola {{1}}, recordatorio: tu plan anual de Tinta vence el {{2}}. Aquí tu ficha para pagar en OXXO: {{3}}
```
`{{1}}`=gym_name · `{{2}}`=expires_on · `{{3}}`=voucher_url

---

#### `oxxo_renewal_reminder_3d`
```
Hola {{1}}, tu plan vence el {{2}} (en 3 días). Paga tu ficha en OXXO para no interrumpir el servicio: {{3}}
```
`{{1}}`=gym_name · `{{2}}`=expires_on · `{{3}}`=voucher_url

---

#### `oxxo_renewal_reminder_today`
```
Hola {{1}}, tu plan vence HOY. Paga tu ficha en OXXO cuanto antes para no perder el servicio: {{2}}
```
`{{1}}`=gym_name · `{{2}}`=voucher_url

---

### MARKETING (1 plantilla)

#### `broadcast_freeform`
```
Hola {{1}}, {{2}} — {{3}}
```
`{{1}}`=member_first_name · `{{2}}`=message · `{{3}}`=gym_name

---

### AUTHENTICATION (2 plantillas)

#### `operator_welcome_pin`
```
Hola {{1}} 👋 {{2}} te dio de alta en Tinta. Tu PIN de acceso es *{{3}}*. Lo usas en el sistema del gym para iniciar sesión.
```
`{{1}}`=full_name · `{{2}}`=gym_name · `{{3}}`=pin

---

#### `whatsapp_connect_otp`
```
Tu código de verificación de Cuadra es {{1}}. Vence en 10 minutos.
```
`{{1}}`=code

> ⚠️ Este template se envía desde el **número master de Cuadra** (no desde el número del gym).
> Crearlo en la cuenta Twilio de Cuadra, no en la del cliente.

## Paso 3 — Obtener credenciales de Twilio

```
Twilio Console → Account → API keys & tokens
   → Account SID:    AC................. (ya está, cópialo)
   → Auth Token:     <click en "View" para revelarlo>
```

Y el WhatsApp number formateado:

```
Twilio Console → Messaging → Senders → WhatsApp Senders
   → tu número aprobado → copy
   → formato: "whatsapp:+5215512345678"
```

## Paso 4 — Pegar credenciales en `.env.prod`

```bash
# Edit local
$EDITOR ~/Documents/Personal/Cuadra/cuadra-core/.env.prod
```

Reemplaza:

```env
WHATSAPP_PROVIDER=twilio                                       # antes: stdout
TWILIO_ACCOUNT_SID=ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
TWILIO_AUTH_TOKEN=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
TWILIO_WHATSAPP_NUMBER=whatsapp:+5215512345678                  # tu número aprobado
TWILIO_WEBHOOK_URL=https://api.entinta.app/api/v1/webhooks/twilio
```

Subir + reiniciar:

```bash
cd ~/Documents/Personal/Cuadra/cuadra-core
SERVER=204.168.214.238 SSH_USER=root bash scripts/deploy/upload-env.sh .env.prod
```

## Paso 5 — Configurar el webhook en Twilio

Twilio necesita saber a dónde mandarte status updates (delivered, read, failed):

```
Twilio Console → Messaging → Senders → WhatsApp Senders
   → tu número → Configure
      Status Callback URL: https://api.entinta.app/api/v1/webhooks/twilio
      HTTP Method: POST
      → Save
```

`tinta-server` ya tiene el handler en `src/modules/notifications/interfaces/controllers/webhook_controller.go`. Verifica firma con `MERCADOPAGO_WEBHOOK_SECRET`... ojo, Twilio usa otro secret:

```
Twilio Console → Settings → Auth Tokens & API Keys → Webhook signature
   (Twilio firma con tu Auth Token estándar — ya lo tienes en TWILIO_AUTH_TOKEN.
   El handler valida `X-Twilio-Signature` con ese mismo token.)
```

No necesitás un secret separado para webhooks de Twilio.

## Paso 6 — Smoke test

```bash
# Trigger manual: dispara un recordatorio de pago a un socio de prueba.
# Si todavía no tienes socios, agrega uno desde el desktop con tu propio
# número antes de probar.

# Tail logs en otra terminal
ssh tinta@204.168.214.238 'sudo journalctl -u tinta-server -f | grep -i twilio'
```

Esperas logs tipo:

```
twilio: sending template expiry_reminder_3d to whatsapp:+521555...
twilio: 201 Created sid=SMxxxxxxxx
twilio: webhook received status=delivered sid=SMxxxxxxxx
```

Y en tu celular: el WhatsApp llega.

## Costos

Twilio precio por mensaje en MX (2026):

| Tipo | Costo |
|---|---|
| Utility template (recordatorios) | ~$0.005 USD = ~$0.10 MXN |
| Marketing template (cumpleaños, broadcasts) | ~$0.030 USD = ~$0.60 MXN |
| Sesión 24h (responder mensaje del cliente) | gratis |

Estimado para un gym con 100 socios:
- Recordatorios mensuales: 100 × 1 = 100 utility → $10 MXN/mes
- Cumpleaños: ~10/mes promedio → $6 MXN/mes
- **Total: ~$16 MXN/mes** por gym, plenamente absorbido en el plan Plus ($1,199 MXN/mes).

Plan Standard incluye solo recordatorios utility (~$10 MXN/mes/gym), también absorbidos.

Por eso Tinta carga el costo de WhatsApp al plan, sin pasar la factura al cliente — es predecible y barato.

## Troubleshooting

**Mensajes no llegan, status `failed` en Twilio dashboard:**
- Template no aprobado por Meta → revisa Content Template Builder
- Número del destinatario no tiene WhatsApp registrado
- Número del destinatario lo bloqueó (después de 1+ mensajes ignorados, Meta lo bloquea automático)

**`401 Unauthorized` en logs:**
- `TWILIO_AUTH_TOKEN` mal copiado. Re-cópialo de Twilio dashboard.

**Status Callback no llega al server:**
- Caddy bloqueando? Verifica `journalctl -u caddy | grep webhooks`
- URL pública mal seteada? `dig +short api.entinta.app`

**Tarifa más alta de lo esperado:**
- Mira el detail en Twilio billing — algún template puede estar marcado como Marketing en lugar de Utility
- Verifica que NO estés mandando texto libre (cuando Twilio tiene que crear "session" automático para responder al socio, todo OK; cuando es outbound first-time, debe ser HSM template)
