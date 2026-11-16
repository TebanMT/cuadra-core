# Deploy de tinta-server a Hetzner

Pipeline para llevar `tinta-server` (API cloud) de cero a producción
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
   ├── Caddy   :80 :443        → reverse proxy api.entinta.app
   │     │
   │     ▼
   ├── tinta-server :8080     ← Go binary, systemd service
   │     │
   │     ▼
   ├── Postgres 16  :5432      ← localhost only
   │
   └── pg_dump → Cloudflare R2 ← tinta-backup.timer diario 03:15 UTC
                                  (rol tinta_backup, no el de la API)
```

DNS: `api.entinta.app A 204.168.214.238` (record proxied=OFF en Cloudflare,
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
usuario `cuadra` con sudo limitado a `systemctl restart tinta-server`,
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
echo "TINTA_SMK_BASE64=$(openssl rand -base64 32)"
```

Crear `tinta-server.env` localmente (NO lo commits — el repo solo tiene
el `.example`):

```bash
cp scripts/deploy/tinta-server.env.example /tmp/tinta-server.env
$EDITOR /tmp/tinta-server.env   # mete DATABASE_URL, JWT_SECRET, SMK, etc.
```

Subirlo al server con permisos correctos:

```bash
scp /tmp/tinta-server.env root@204.168.214.238:/tmp/
ssh root@204.168.214.238 '
  install -o tinta -g tinta -m 600 /tmp/tinta-server.env /opt/tinta/tinta-server.env
  rm /tmp/tinta-server.env
'

# Borrar la copia local también — fue temporal.
rm /tmp/tinta-server.env
```

### 5. Instalar systemd unit + Caddyfile

```bash
SERVER=204.168.214.238 bash scripts/deploy/install-system-files.sh
```

> Antes de este paso el DNS `api.entinta.app → 204.168.214.238` ya tiene
> que estar propagado. Caddy va a sacar el cert TLS la primera vez que
> alguien hace HTTP a `api.entinta.app`. Verifica con `dig api.entinta.app`.

### 6. Primer deploy del binario

```bash
SERVER=204.168.214.238 bash scripts/deploy/02-deploy-binary.sh
```

Esto cross-compila para `linux/amd64`, sube el binario, corre las
migraciones SQL, y reinicia el servicio. Logs:

```bash
ssh tinta@204.168.214.238 'sudo journalctl -u tinta-server -f'
```

Smoke test desde tu laptop:

```bash
curl -i https://api.entinta.app/health
```

### 7. Backups a R2 (obligatorio antes de tener gimnasios reales)

Desde la migración 057 las tablas tienen RLS y el rol de la API
(`tinta_app`) no puede saltarla: `pg_dump` con él falla con
`query would be affected by row-level security policy`. El respaldo usa
su propio rol, `tinta_backup` (solo lectura + `BYPASSRLS`), con la
credencial en `/etc/tinta/backup.env` (root, 0600). Solo la recibe
`tinta-backup.service`; nunca va a `/opt/tinta/tinta-server.env`.

1. **R2.** En Cloudflare crea el bucket privado `tinta-backups` y un API
   token "Object Read & Write" limitado a ese bucket. Anota access key,
   secret y endpoint `https://<account-id>.r2.cloudflarestorage.com`.
   Agrega lifecycle rules para la rotación (7 dailies, 4 weeklies, 12
   monthlies).
2. **aws CLI v2** en el servidor (el paquete `awscli` ya no está en
   Ubuntu 24.04):

   ```bash
   ssh root@$SERVER 'cd /tmp && curl -sSLo awscliv2.zip https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip && apt-get -y install unzip && unzip -q awscliv2.zip && ./aws/install && rm -rf aws awscliv2.zip && aws --version'
   ```
3. **Archivos de sistema** (usuario `tinta-backup`, script en
   `/usr/local/sbin/tinta-backup`, service y timer apagado):

   ```bash
   SERVER=204.168.214.238 bash scripts/deploy/install-system-files.sh
   ```
4. **Rol y credencial.** Crea `tinta_backup`, escribe
   `/etc/tinta/backup.env` y prueba un dump completo sin subirlo:

   ```bash
   SERVER=204.168.214.238 bash scripts/deploy/04-backup-role.sh
   ```
5. **Variables de R2.** Agrégalas a `/etc/tinta/backup.env`
   (`sudo nano /etc/tinta/backup.env`): `R2_ENDPOINT`, `R2_BUCKET`,
   `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`. Vuelve a correr el paso 4:
   esta vez activa el timer.
6. **Primer respaldo real** y revisión:

   ```bash
   ssh root@$SERVER 'systemctl start tinta-backup && journalctl -u tinta-backup -n 20 --no-pager'
   ```

   Debe terminar en `✓ backup en R2`. Después: `systemctl list-timers
   tinta-backup.timer` muestra la próxima corrida.
7. **Prueba de restauración** (una vez al mes, y siempre antes de abrir
   ventas). Descarga el último `.sql.gz` y restáuralo en una base vacía
   que no sea la de producción:

   ```bash
   createdb tinta_restore_check
   gzip -dc tinta-<fecha>.sql.gz | psql -X -v ON_ERROR_STOP=1 -d tinta_restore_check
   psql -d tinta_restore_check -c "SELECT count(*) FROM gyms; SELECT max(version) FROM _migrations;"
   dropdb tinta_restore_check
   ```

Antes de cada dump el script comprueba que el rol tenga `BYPASSRLS`, y
después que el `.gz` esté íntegro, que el dump termine completo y que
traiga `gyms`. Si algo falla, el servicio termina con error y
`systemctl status tinta-backup` lo muestra. **Todavía no hay alerta
automática**: revisa el journal o los objetos del bucket cada semana
hasta conectar una.

> Si en el servidor ya existía el cron viejo de `03-backup.sh` (usuario
> `tinta`, `/opt/tinta/bin/backup.sh`), bórralo con `crontab -u tinta -e`:
> fallaba cada noche por RLS.

---

## Redeploys de rutina

El camino normal es un tag: `git tag v1.4.0 && git push origin v1.4.0`
dispara `.github/workflows/release.yml`. Para una emergencia sin Actions:

```bash
SERVER=204.168.214.238 bash scripts/deploy/02-deploy-binary.sh
```

Los dos hacen lo mismo:

1. Compilan `tinta-server` y `tinta-migrate` (linux/amd64, sin CGO) **de un
   commit**. `02-deploy-binary.sh` exporta `REF` (default `HEAD`) con
   `git archive`: lo que no está en commit no viaja, aunque esté en tu
   carpeta.
2. Suben los binarios como `.new` y el SQL de ese commit a
   `/opt/tinta/migrations`, sin tocar la API que está corriendo.
3. `scripts/deploy/remote-release.sh`, en el servidor:
   1. Respaldo previo (`tinta-backup`) si el timer de respaldos está activo.
   2. Migraciones con el rol dueño (`tinta-migrate.service`). **Si fallan,
      la API anterior sigue igual**, en disco y en memoria.
   3. Guarda la API actual como `tinta-server.prev`, cambia a la nueva y
      reinicia.
   4. Espera ~12 s a que `http://127.0.0.1:8080/health` reporte la versión
      nueva. Si no llega, regresa sola a `tinta-server.prev`, deja la que
      falló en `tinta-server.failed` y el deploy termina con error.

Las migraciones que ya se aplicaron no se revierten: tienen que ser
aditivas, porque la API anterior convive con ellas desde el paso 3.2.

Flags útiles:

```bash
# Desplegar un tag concreto (o regresar a uno anterior).
SERVER=... REF=v1.3.0 bash scripts/deploy/02-deploy-binary.sh

# Solo redeploy del binario, sin respaldo ni SQL.
SERVER=... SKIP_MIGRATE=1 bash scripts/deploy/02-deploy-binary.sh

# Subir todo pero sin activar (p.ej. para precargar antes de un corte de
# mantenimiento). El script imprime el comando para activarlo después.
SERVER=... SKIP_RESTART=1 bash scripts/deploy/02-deploy-binary.sh
```

> `remote-release.sh` usa `sudo -n` para el respaldo previo, el rollback y
> los logs (`/etc/sudoers.d/tinta-deploy`). Si el servidor se instaló antes
> de este cambio, vuelve a correr `install-system-files.sh` una vez.

## Cambios de configuración del sistema

Si tocas `tinta-server.service` o el `Caddyfile` en este repo:

```bash
SERVER=204.168.214.238 bash scripts/deploy/install-system-files.sh
```

Si tocas el `.env` en el server: nada automatizado — edítalo a mano y
`sudo systemctl restart tinta-server`.

## Troubleshooting rápido

```bash
# ¿Está corriendo el server?
ssh tinta@$SERVER 'sudo systemctl status tinta-server'

# Logs en vivo
ssh tinta@$SERVER 'sudo journalctl -u tinta-server -f'

# Logs de Caddy (TLS, requests cortados)
ssh tinta@$SERVER 'sudo journalctl -u caddy -f'

# ¿Postgres responde?
ssh tinta@$SERVER 'sudo -u postgres psql -c "\l" | grep cuadra'

# ¿El binario corre a mano? (debug de configuración)
ssh tinta@$SERVER '
  source /opt/tinta/tinta-server.env
  /opt/tinta/bin/tinta-server
'
```

## Rollback

Cada deploy sobrescribe `/opt/tinta/bin/tinta-server`. No hay
versiones; si necesitas volver atrás, build con el commit anterior y
re-deploy. Si el rollback es urgente:

```bash
git checkout <prev-commit>
SERVER=$SERVER SKIP_MIGRATE=1 bash scripts/deploy/02-deploy-binary.sh
git checkout main
```

(SKIP_MIGRATE porque las migraciones son forward-only — no hay `down`.
Si el rollback necesita revertir schema, eso es manual y caso por caso.)
