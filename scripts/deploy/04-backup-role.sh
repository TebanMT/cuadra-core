#!/usr/bin/env bash
# 04-backup-role.sh — rol de Postgres para respaldos + /etc/tinta/backup.env.
#
# Se ejecuta DESDE TU LAPTOP. Hace ssh al servidor como root. Antes corre
# install-system-files.sh (usuario tinta-backup, script, service y timer).
#
# Por qué: desde la migración 057 las tablas tienen RLS y el rol de la API
# (tinta_app) no puede saltarla, así que pg_dump con él falla. El respaldo
# necesita un rol que lea todo, y esa credencial no debe llegar a la API.
#
# Qué hace (idempotente):
#   1. Crea/ajusta el rol tinta_backup: LOGIN, BYPASSRLS, NOSUPERUSER,
#      NOCREATEDB, NOCREATEROLE, NOREPLICATION; solo lectura vía
#      pg_read_all_data. Verifica que no sea dueño de tablas.
#   2. Escribe BACKUP_DATABASE_URL en /etc/tinta/backup.env (root, 0600) y
#      conserva el resto del archivo (las variables de R2). Cada corrida
#      rota la contraseña; la genera el servidor y no pasa por tu terminal.
#   3. Prueba un dump completo, sin subirlo, como el usuario tinta-backup.
#   4. Si las variables de R2 ya están en el archivo, activa el timer.
#
# Uso:
#   SERVER=204.168.214.238 bash scripts/deploy/04-backup-role.sh
#
# La primera vez termina pidiendo las variables de R2. Agrégalas a
# /etc/tinta/backup.env y vuelve a correrlo para activar el timer.

set -euo pipefail

: "${SERVER:?Falta SERVER (ip o hostname del Hetzner)}"
SSH_USER="${SSH_USER:-root}"
DB_NAME="${DB_NAME:-tinta}"
BACKUP_ROLE="${BACKUP_ROLE:-tinta_backup}"

for name in "${DB_NAME}" "${BACKUP_ROLE}"; do
  [[ "${name}" =~ ^[a-z_][a-z0-9_]*$ ]] || { echo "✗ Nombre inválido: ${name}" >&2; exit 1; }
done

echo "→ Preparando el rol ${BACKUP_ROLE} en ${SERVER}..."

ssh "${SSH_USER}@${SERVER}" "DB_NAME=${DB_NAME} BACKUP_ROLE=${BACKUP_ROLE} bash -s" <<'EOF'
set -euo pipefail
umask 077
cd /
# Este script llega por stdin (bash -s): todo comando que pudiera leer stdin
# va con </dev/null para no tragarse el resto del script.

for f in /usr/local/sbin/tinta-backup /etc/systemd/system/tinta-backup.service /etc/systemd/system/tinta-backup.timer; do
  [ -e "$f" ] || { echo "✗ Falta $f — corre install-system-files.sh primero." >&2; exit 1; }
done
id tinta-backup >/dev/null 2>&1 || { echo "✗ Falta el usuario tinta-backup — corre install-system-files.sh primero." >&2; exit 1; }

ENV_FILE=/etc/tinta/backup.env
install -d -o root -g root -m 700 /etc/tinta
PW=$(openssl rand -hex 24)
NEW=$(mktemp /etc/tinta/.backup.env.XXXXXX)
trap 'rm -f "$NEW"' EXIT
{
  echo "# /etc/tinta/backup.env: root:root 0600. Solo para tinta-backup.service; nunca al entorno de la API."
  echo "BACKUP_DATABASE_URL=postgresql://${BACKUP_ROLE}:${PW}@127.0.0.1:5432/${DB_NAME}?sslmode=disable"
  if [ -f "$ENV_FILE" ]; then
    grep -v -e '^BACKUP_DATABASE_URL=' -e '^# /etc/tinta/backup.env:' "$ENV_FILE" || true
  else
    cat <<'TPL'
# Cloudflare R2 — token "Object Read & Write" limitado al bucket.
#R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
#R2_BUCKET=tinta-backups
#AWS_ACCESS_KEY_ID=
#AWS_SECRET_ACCESS_KEY=
TPL
  fi
} > "$NEW"
chown root:root "$NEW"
chmod 600 "$NEW"

# La contraseña va por stdin, no por argv. Los nombres ya vienen validados.
sudo -u postgres psql -X -q -v ON_ERROR_STOP=1 -d "${DB_NAME}" <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${BACKUP_ROLE}') THEN
    CREATE ROLE ${BACKUP_ROLE} LOGIN;
  END IF;
END \$\$;
ALTER ROLE ${BACKUP_ROLE} LOGIN BYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION INHERIT PASSWORD '${PW}';
GRANT pg_read_all_data TO ${BACKUP_ROLE};
GRANT CONNECT ON DATABASE ${DB_NAME} TO ${BACKUP_ROLE};
SQL
mv "$NEW" "$ENV_FILE"
trap - EXIT

# Solo lectura de verdad: sin superusuario, con pg_read_all_data y sin ser
# (ni heredar) dueño de ninguna tabla.
OK=$(sudo -u postgres psql -X -A -t -v ON_ERROR_STOP=1 -d "${DB_NAME}" -c "
  SELECT r.rolbypassrls AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole
     AND pg_has_role(r.oid, 'pg_read_all_data', 'USAGE')
     AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.relkind IN ('r','p') AND pg_has_role(r.oid, c.relowner, 'MEMBER'))
  FROM pg_roles r WHERE r.rolname = '${BACKUP_ROLE}'" </dev/null)
[ "$OK" = "t" ] || { echo "✗ ${BACKUP_ROLE} no quedó como rol de solo lectura con BYPASSRLS." >&2; exit 1; }
echo "✓ Rol ${BACKUP_ROLE} listo; credencial en ${ENV_FILE}."

echo "→ Probando el dump como tinta-backup (sin subir a R2)..."
systemd-run --quiet --wait --pipe --collect \
  -p User=tinta-backup -p Group=tinta-backup \
  -p EnvironmentFile="${ENV_FILE}" \
  -p PrivateTmp=yes -p ProtectSystem=strict -p ProtectHome=yes -p NoNewPrivileges=yes \
  --setenv=HOME=/tmp --setenv=BACKUP_DRY_RUN=1 \
  /usr/local/sbin/tinta-backup </dev/null

if ! command -v aws >/dev/null 2>&1; then
  echo "⚠ El dump funciona, pero falta aws CLI v2 en el servidor (ver README, 'Backups a R2')."
  echo "  Instálala y vuelve a correr este script para activar el timer."
elif grep -qE '^R2_ENDPOINT=.+' "$ENV_FILE" &&
   grep -qE '^AWS_ACCESS_KEY_ID=.+' "$ENV_FILE" &&
   grep -qE '^AWS_SECRET_ACCESS_KEY=.+' "$ENV_FILE"; then
  systemctl daemon-reload
  systemctl enable --now tinta-backup.timer
  echo "✓ Timer activo:"
  systemctl list-timers tinta-backup.timer --no-pager | sed -n 1,2p
  echo "  Primer respaldo real: sudo systemctl start tinta-backup && journalctl -u tinta-backup -n 20"
else
  echo "⚠ El dump funciona, pero faltan las variables de R2 en ${ENV_FILE}:"
  echo "    R2_ENDPOINT, R2_BUCKET, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY"
  echo "  Agrégalas (sudo nano ${ENV_FILE}) y vuelve a correr este script para activar el timer."
fi
EOF
