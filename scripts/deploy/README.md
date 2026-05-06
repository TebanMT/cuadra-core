# Deploy de cuadra-server a Hetzner

Pipeline para llevar `cuadra-server` (API cloud) de cero a producción
en un Hetzner CX33 fresh con Ubuntu 24.04.

> Si solo vas a hacer **redeploy de un cambio de código** después del
> primer setup: salta directo a [Redeploys de rutina](#redeploys-de-rutina).

## Topología

```
laptop (tú)
   │
   │  ssh root @ primer setup
   │  ssh cuadra @ deploys de rutina
   ▼
Hetzner CX33 (Helsinki)        ← public IP, ufw 22/80/443
   ├── Caddy   :80 :443        → reverse proxy api.cuadra.app
   │     │
   │     ▼
   ├── cuadra-server :8080     ← Go binary, systemd service
   │     │
   │     ▼
   ├── Postgres 16  :5432      ← localhost only
   │
   └── pg_dump → Cloudflare R2 ← cron diario 03:15 UTC
```

DNS: `api.cuadra.app A 204.168.214.238` (record proxied=OFF en Cloudflare,
porque Caddy ya saca cert Let's Encrypt — si lo proxeas, el HTTP-01
challenge se rompe a menos que actives "Full strict" + cert).

## Primera vez (setup completo)

### 1. Hetzner: provisionar el CX33

- Image: Ubuntu 24.04
- Location: Helsinki (HEL1) — el CX33 es el único disponible ahí.
- SSH key: la que generaste con `ssh-keygen -t rsa -b 4096`.

Anotar la public IP. En este repo asumimos `204.168.214.238`.

### 2. Bootstrap (sistema base)

Desde tu laptop:

```bash
scp scripts/deploy/00-bootstrap.sh root@204.168.214.238:/tmp/
ssh root@204.168.214.238 'bash /tmp/00-bootstrap.sh'
```

Esto instala Caddy, Postgres 16, Go 1.25.3, ufw + fail2ban, crea el
usuario `cuadra` con sudo limitado a `systemctl restart cuadra-server`,
y endurece SSH (no root password, no password auth).

### 3. Postgres: crear DB

```bash
SERVER=204.168.214.238 \
  DB_PASSWORD="$(openssl rand -base64 32 | tr -d '/+=' | head -c 32)" \
  bash scripts/deploy/01-create-db.sh
```

**Anota la `DATABASE_URL` que imprime.** La vas a meter en el `.env`
en el siguiente paso.

### 4. Subir el `.env` con secrets

Generar los secretos en tu laptop:

```bash
echo "JWT_SECRET=$(openssl rand -base64 64 | tr -d '\n')"
echo "CUADRA_SMK_BASE64=$(openssl rand -base64 32)"
```

Crear `cuadra-server.env` localmente (NO lo commits — el repo solo tiene
el `.example`):

```bash
cp scripts/deploy/cuadra-server.env.example /tmp/cuadra-server.env
$EDITOR /tmp/cuadra-server.env   # mete DATABASE_URL, JWT_SECRET, SMK, etc.
```

Subirlo al server con permisos correctos:

```bash
scp /tmp/cuadra-server.env root@204.168.214.238:/tmp/
ssh root@204.168.214.238 '
  install -o cuadra -g cuadra -m 600 /tmp/cuadra-server.env /opt/cuadra/cuadra-server.env
  rm /tmp/cuadra-server.env
'

# Borrar la copia local también — fue temporal.
rm /tmp/cuadra-server.env
```

### 5. Instalar systemd unit + Caddyfile

```bash
SERVER=204.168.214.238 bash scripts/deploy/install-system-files.sh
```

> Antes de este paso el DNS `api.cuadra.app → 204.168.214.238` ya tiene
> que estar propagado. Caddy va a sacar el cert TLS la primera vez que
> alguien hace HTTP a `api.cuadra.app`. Verifica con `dig api.cuadra.app`.

### 6. Primer deploy del binario

```bash
SERVER=204.168.214.238 bash scripts/deploy/02-deploy-binary.sh
```

Esto cross-compila para `linux/amd64`, sube el binario, corre las
migraciones SQL, y reinicia el servicio. Logs:

```bash
ssh cuadra@204.168.214.238 'sudo journalctl -u cuadra-server -f'
```

Smoke test desde tu laptop:

```bash
curl -i https://api.cuadra.app/health
```

### 7. Backups a R2 (opcional pero recomendado)

Lee el comentario de `03-backup.sh` — explica el setup paso a paso:
crear bucket, API token, `~/.aws/credentials`, cron entry.

---

## Redeploys de rutina

Para cualquier cambio de código en `cuadra-core`:

```bash
SERVER=204.168.214.238 bash scripts/deploy/02-deploy-binary.sh
```

Eso es todo. El script:

1. `GOOS=linux GOARCH=amd64 go build` (sin CGO).
2. `scp` del binario a `${REMOTE_BIN}.new` y rotación atómica.
3. `rsync` de las migraciones SQL.
4. `psql -f` de cada migración nueva (idempotentes — el server las
   chequea por nombre, así que repetir está bien si las hiciste así).
5. `sudo systemctl restart cuadra-server`.
6. Smoke check al endpoint local.

Flags útiles:

```bash
# Solo redeploy del binario, sin tocar SQL.
SERVER=... SKIP_MIGRATE=1 bash scripts/deploy/02-deploy-binary.sh

# Subir todo pero sin reiniciar (p.ej. para precargar antes de un
# corte de mantenimiento).
SERVER=... SKIP_RESTART=1 bash scripts/deploy/02-deploy-binary.sh
```

## Cambios de configuración del sistema

Si tocas `cuadra-server.service` o el `Caddyfile` en este repo:

```bash
SERVER=204.168.214.238 bash scripts/deploy/install-system-files.sh
```

Si tocas el `.env` en el server: nada automatizado — edítalo a mano y
`sudo systemctl restart cuadra-server`.

## Troubleshooting rápido

```bash
# ¿Está corriendo el server?
ssh cuadra@$SERVER 'sudo systemctl status cuadra-server'

# Logs en vivo
ssh cuadra@$SERVER 'sudo journalctl -u cuadra-server -f'

# Logs de Caddy (TLS, requests cortados)
ssh cuadra@$SERVER 'sudo journalctl -u caddy -f'

# ¿Postgres responde?
ssh cuadra@$SERVER 'sudo -u postgres psql -c "\l" | grep cuadra'

# ¿El binario corre a mano? (debug de configuración)
ssh cuadra@$SERVER '
  source /opt/cuadra/cuadra-server.env
  /opt/cuadra/bin/cuadra-server
'
```

## Rollback

Cada deploy sobrescribe `/opt/cuadra/bin/cuadra-server`. No hay
versiones; si necesitas volver atrás, build con el commit anterior y
re-deploy. Si el rollback es urgente:

```bash
git checkout <prev-commit>
SERVER=$SERVER SKIP_MIGRATE=1 bash scripts/deploy/02-deploy-binary.sh
git checkout main
```

(SKIP_MIGRATE porque las migraciones son forward-only — no hay `down`.
Si el rollback necesita revertir schema, eso es manual y caso por caso.)
