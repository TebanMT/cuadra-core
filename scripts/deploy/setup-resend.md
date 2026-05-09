# Resend setup para Tinta (envío transaccional)

Tinta usa Resend para mandar correos transaccionales: bienvenida, recuperación de contraseña, recordatorios de vencimiento al socio, alertas al dueño. El backend Go ya tiene el adapter — solo falta:

1. Crear cuenta en Resend
2. Verificar el dominio `entinta.mx`
3. Agregar 3 records DNS en Cloudflare
4. Pegar el `RESEND_API_KEY` en `.env.prod`
5. Subir el `.env` actualizado al server

Tiempo total: ~10 minutos.

## Por qué `entinta.mx` y no `entinta.app`

Email tradicionalmente vive bajo el dominio "principal" / marketing del producto. Email enviado desde `noreply@entinta.app` se ve raro porque `.app` connota app/producto, no comunicación. Mantenemos:

- `noreply@entinta.mx` para envío transaccional
- `soporte@entinta.mx`, `legal@entinta.mx`, `privacidad@entinta.mx` para inbound

Resend solo necesita verificar `entinta.mx` (domain-level), después podés mandar desde cualquier alias `@entinta.mx`.

## Paso 1 — Crear cuenta

```
1. Andá a https://resend.com → Sign up
2. Confirmás email, te aterriza al dashboard
3. Sidebar → Domains → Add Domain
   Domain: entinta.mx
   Region: us-east-1 (más barato y suficiente latencia)
   → Add
```

Resend te muestra una pantalla con **3 records DNS** que tenés que agregar:

```
Type   Name                  Content
─────  ────────────────────  ──────────────────────────────
TXT    send.entinta.mx       v=spf1 include:amazonses.com ~all
TXT    resend._domainkey     <DKIM key larga>
MX     send.entinta.mx       feedback-smtp.us-east-1.amazonses.com (priority 10)
```

(Los nombres y valores exactos varían — usá los que te muestra TU panel de Resend, no estos como copy-paste literal.)

## Paso 2 — Agregar records en Cloudflare

```
Cloudflare Dashboard → entinta.mx → DNS → Records → Add record
```

Para cada record que Resend te dio:

| Campo | Qué poner |
|---|---|
| Type | igual que dice Resend (TXT, MX, etc) |
| Name | igual que dice Resend (sin agregar `.entinta.mx` — CF agrega solo) |
| Content | el valor exacto de Resend |
| Proxy | **OFF (gris)** — los records de email NO se proxean |
| TTL | Auto |

> **Importante**: para records TXT donde el `Content` empieza con `v=spf1`, **no es** un nuevo SPF — es un include para Resend. Si ya tenés un SPF para `entinta.mx`, hay que combinarlos en un solo record. Si no había, agregá el de Resend tal cual.

## Paso 3 — Verificar en Resend

Volvé al panel de Resend → Domains → `entinta.mx` → click **Verify DNS records**.

Tiempos:
- DNS propagación: **~30 seg** desde CF
- Resend verifica: **~1 min** después de eso

Cuando los 3 records aparecen como `Verified`, el dominio queda activo y podés mandar mails desde cualquier `*@entinta.mx`.

## Paso 4 — Crear API key

```
Resend Dashboard → API Keys → Create API Key
   Name: tinta-prod
   Permission: Sending access (no full access)
   Domain: entinta.mx (limitar a este dominio)
   → Create
```

**Te muestra el secret una sola vez**. Copialo. Empieza con `re_`.

## Paso 5 — Subir el secret al server

En tu laptop:

```bash
# Edit .env.prod local — agregar/actualizar la línea:
echo "RESEND_API_KEY=re_xxxxxxxxxxxxxxxxxxxx" >> ~/Documents/Personal/Cuadra/cuadra-core/.env.prod
# (Mejor: editá el archivo a mano y reemplazá la línea existente que estaba vacía.)

# Subir + reiniciar service
cd ~/Documents/Personal/Cuadra/cuadra-core
SERVER=204.168.214.238 SSH_USER=root bash scripts/deploy/upload-env.sh .env.prod
```

`upload-env.sh` ya valida que las vars críticas estén presentes y reinicia `tinta-server` automático.

## Paso 6 — Smoke test

```bash
# Trigger manual: forgot-password manda un correo
curl -X POST https://api.entinta.app/api/v1/auth/forgot-password \
  -H "Content-Type: application/json" \
  -d '{"email":"tu-correo-personal@gmail.com"}'

# Esperá ~10 segundos y revisá tu inbox.
# Si llega: Resend OK.
# Si NO llega:
ssh tinta@204.168.214.238 'sudo journalctl -u tinta-server -n 50 | grep -i resend'
```

Logs típicos a buscar:

```
✓ resend: 200 OK    (mail aceptado)
✗ resend: 401 unauthorized  (API key incorrecta)
✗ resend: 422 unprocessable (FROM no es del dominio verificado)
```

## Mantenimiento

- **Rotar API key**: crear key nueva en Resend → actualizar `.env.prod` → `upload-env.sh` → revocar key vieja en Resend dashboard.
- **Cambiar el `EMAIL_FROM`**: editar `.env.prod` (default es `noreply@entinta.mx`), upload, restart.
- **Monitor**: Resend dashboard tiene Activity log con todos los sends, bounces, opens. Útil para debug.

## Costos

Resend free tier:
- **3,000 emails/mes gratis**
- 100/día max
- Dominios y API keys ilimitados

Para Tinta es más que suficiente al arranque. Si ~20 gyms cada uno mandando recordatorios diarios = 600/mes. Margen amplio.

Plan paid empieza en $20/mo por 50k emails/mes (cuando llegues a ese volumen, ~1500 gyms operando).
