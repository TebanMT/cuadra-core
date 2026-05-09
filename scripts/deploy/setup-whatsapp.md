# WhatsApp setup para Tinta (envío vía Twilio)

Tinta usa Twilio para WhatsApp porque:

1. Twilio tiene WhatsApp Business API oficial (no es scraper, no se rompe)
2. Soporta plantillas pre-aprobadas (HSM templates) que es lo que Meta exige para mensajes proactivos
3. SDK Go robusto, ya integrado en `cuadra-core/src/modules/notifications/infraestructure/whatsapp/twilio.go`
4. Costos predecibles: ~$0.005 USD por mensaje en MX
5. Twilio te puede asignar un número y maneja el flow de aprobación con Meta

Alternativas como WhatsApp Business directo (Cloud API de Meta) son más baratas pero MUCHO más fricción de setup. Twilio vale la pena.

Tiempo total: depende de Meta (1-3 días de revisión cuando registrás el número/business). Una vez aprobado, configurar las env vars en Tinta toma ~5 min.

## Pre-requisitos (lo que estás haciendo ahora)

1. **Cuenta WhatsApp Business** vinculada a un número que NO uses para WhatsApp personal. Idealmente un chip nuevo.
2. **Verificación del Meta Business Manager** — Meta exige que tu negocio esté verificado para mandar mensajes "outbound" iniciados (templates HSM). Verificación = subir docs (acta constitutiva, RFC, identificación, etc.) y esperar revisión.
3. **Cuenta Twilio** activada con métodos de pago.

Si no terminaste alguno de los 3 todavía, **terminá esos primero**. Lo que sigue asume que ya tenés el número WhatsApp Business operativo y la cuenta Twilio creada.

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

Mientras esperás aprobación, podés seguir desarrollando con el Twilio Sandbox (un número compartido `+1 415 523 8886` con prefijo). Útil para testing pero no para producción.

## Paso 2 — Crear plantillas (HSM templates) en Meta

WhatsApp Business **prohíbe** mandar mensajes outbound iniciados con texto libre. Tenés que usar **plantillas pre-aprobadas** por Meta. Tinta necesita al menos estas:

### Template `tinta_payment_reminder`

```
Hola {{1}}, te recordamos que tu mensualidad de {{2}} vence el {{3}}. 
Renovala desde la app o pasando al gimnasio. ¡Gracias!
```

Variables:
- `{{1}}` = nombre del socio
- `{{2}}` = nombre del plan (ej. "Mensualidad")
- `{{3}}` = fecha de vencimiento (ej. "5 de mayo")

Categoría: **UTILITY** (es recordatorio operacional, no marketing).

### Template `tinta_birthday_greeting`

```
¡Feliz cumpleaños, {{1}}! 🎉 De parte del equipo de {{2}}, 
te deseamos un gran día. Que la rompas en este nuevo año.
```

Variables: `{{1}}` = nombre, `{{2}}` = nombre del gimnasio. Categoría: **MARKETING**.

### Template `tinta_welcome_member`

```
Hola {{1}}, ¡bienvenido/a a {{2}}! 
Tu membresía está activa y vence el {{3}}. 
Cualquier duda escribinos por aquí.
```

Categoría: **UTILITY**.

### Cómo se crean

```
Twilio Console → Messaging → Content Template Builder
   → Create new template
      Friendly Name: tinta_payment_reminder
      Category: UTILITY
      Language: es_MX (Spanish - Mexico)
      Body: <pegás el texto con {{1}}, {{2}}, {{3}}>
      → Submit for approval
```

Meta los aprueba en **1-24h** (utility suele ser rápido, marketing más lento).

## Paso 3 — Obtener credenciales de Twilio

```
Twilio Console → Account → API keys & tokens
   → Account SID:    AC................. (ya está, copialo)
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

Reemplazá:

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
   (Twilio firma con tu Auth Token estándar — ya lo tenés en TWILIO_AUTH_TOKEN.
   El handler valida `X-Twilio-Signature` con ese mismo token.)
```

No necesitás un secret separado para webhooks de Twilio.

## Paso 6 — Smoke test

```bash
# Trigger manual: dispará un recordatorio de pago a un socio de prueba.
# Si todavía no tenés socios, agregá uno desde el desktop con tu propio
# número antes de probar.

# Tail logs en otra terminal
ssh tinta@204.168.214.238 'sudo journalctl -u tinta-server -f | grep -i twilio'
```

Esperás logs tipo:

```
twilio: sending template tinta_payment_reminder to whatsapp:+521555...
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
- Template no aprobado por Meta → revisá Content Template Builder
- Número del destinatario no tiene WhatsApp registrado
- Número del destinatario lo bloqueó (después de 1+ mensajes ignorados, Meta lo bloquea automático)

**`401 Unauthorized` en logs:**
- `TWILIO_AUTH_TOKEN` mal copiado. Re-copialo de Twilio dashboard.

**Status Callback no llega al server:**
- Caddy bloqueando? Verificá `journalctl -u caddy | grep webhooks`
- URL pública mal seteada? `dig +short api.entinta.app`

**Tarifa más alta de lo esperado:**
- Mirá el detail en Twilio billing — algún template puede estar marcado como Marketing en lugar de Utility
- Verifica que NO estés mandando texto libre (cuando Twilio tiene que crear "session" automático para responder al socio, todo OK; cuando es outbound first-time, debe ser HSM template)
