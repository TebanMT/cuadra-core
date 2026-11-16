#!/usr/bin/env bash
# 03-backup.sh — pg_dump + subida a Cloudflare R2 (S3-compatible).
#
# En el servidor vive como /usr/local/sbin/tinta-backup (root, 0755) y lo
# corre tinta-backup.timer cada noche a las 03:15 UTC (21:15 CDMX, después
# del cierre del gym) como el usuario de sistema `tinta-backup`. Los logs
# quedan en journald: journalctl -u tinta-backup. La rotación (7 dailies +
# 4 weeklies + 12 monthlies) es una lifecycle rule del bucket en R2.
#
# Por qué un rol propio: desde la migración 057 las tablas tienen RLS y el
# rol de la API (tinta_app) no puede saltarla. pg_dump con ese rol falla con
# "query would be affected by row-level security policy". El respaldo usa
# `tinta_backup`: solo lectura (pg_read_all_data) + BYPASSRLS. Su
# credencial vive en /etc/tinta/backup.env (root, 0600), que systemd
# entrega solo a este servicio; la API nunca la ve.
#
# Setup en el servidor (una sola vez): ver "Backups a R2" en
# scripts/deploy/README.md. En resumen:
#   1. install-system-files.sh   — usuario, script, service y timer.
#   2. 04-backup-role.sh         — rol tinta_backup + /etc/tinta/backup.env.
#   3. Agregar las variables de R2 a /etc/tinta/backup.env.
#   4. Volver a correr 04-backup-role.sh: prueba el dump y activa el timer.
#
# Variables (de /etc/tinta/backup.env):
#   BACKUP_DATABASE_URL   — conexión con tinta_backup. Obligatoria. A
#                           propósito NO se cae a DATABASE_URL.
#   R2_ENDPOINT           — https://<acct>.r2.cloudflarestorage.com
#   R2_BUCKET             — nombre del bucket (default tinta-backups).
#   AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
#                         — token de R2 "Object Read & Write" del bucket.
#   R2_PROFILE            — opcional: profile de ~/.aws/credentials en vez
#                           de las dos variables anteriores (corridas a mano).
#
# Opcionales para corridas manuales:
#   BACKUP_ENV_FILE=<ruta> — carga ese archivo antes de empezar.
#   BACKUP_DRY_RUN=1       — hace el dump y lo verifica, sin subirlo.
#   BACKUP_KEEP_DIR=<dir>  — deja una copia del .sql.gz ahí (prueba de restore).

set -euo pipefail

log() { echo "[$(date -u +%FT%TZ)] $*"; }
fail() {
  log "✗ $*" >&2
  exit 1
}

if [ -n "${BACKUP_ENV_FILE:-}" ]; then
  [ -f "${BACKUP_ENV_FILE}" ] || fail "No existe ${BACKUP_ENV_FILE}"
  # shellcheck disable=SC1090
  set -a; source "${BACKUP_ENV_FILE}"; set +a
fi

if [ -z "${BACKUP_DATABASE_URL:-}" ]; then
  fail "Falta BACKUP_DATABASE_URL (rol tinta_backup). No se usa DATABASE_URL: el rol de la API no puede leer a través de RLS. Ver scripts/deploy/04-backup-role.sh."
fi

DRY_RUN="${BACKUP_DRY_RUN:-0}"
R2_BUCKET="${R2_BUCKET:-tinta-backups}"
if [ "${DRY_RUN}" != "1" ]; then
  : "${R2_ENDPOINT:?Falta R2_ENDPOINT (ej: https://abc123.r2.cloudflarestorage.com)}"
  if [ -z "${R2_PROFILE:-}" ] && { [ -z "${AWS_ACCESS_KEY_ID:-}" ] || [ -z "${AWS_SECRET_ACCESS_KEY:-}" ]; }; then
    fail "Faltan AWS_ACCESS_KEY_ID y AWS_SECRET_ACCESS_KEY (token de R2) o R2_PROFILE."
  fi
fi
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-auto}"
AWS_ARGS=(--endpoint-url "${R2_ENDPOINT:-}")
if [ -n "${R2_PROFILE:-}" ]; then
  AWS_ARGS+=(--profile "${R2_PROFILE}")
fi

# Preflight: sin BYPASSRLS el dump falla a la mitad. Mejor un error claro
# antes de empezar.
CAN_READ=$(psql "${BACKUP_DATABASE_URL}" -X -A -t -v ON_ERROR_STOP=1 \
  -c "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user") ||
  fail "No se pudo conectar con BACKUP_DATABASE_URL."
if [ "${CAN_READ}" != "t" ]; then
  fail "El rol de BACKUP_DATABASE_URL no tiene BYPASSRLS: pg_dump no puede leer las tablas con RLS. Corre scripts/deploy/04-backup-role.sh."
fi

umask 077
DATE_UTC=$(date -u +%Y-%m-%dT%H-%M-%SZ)
WORKDIR=$(mktemp -d)
trap 'rm -rf "${WORKDIR}"' EXIT
DUMP_FILE="${WORKDIR}/tinta-${DATE_UTC}.sql.gz"

log "→ pg_dump → ${DUMP_FILE}"
# --no-owner --no-privileges para que el restore sea portable. Formato
# plain + gzip; con pipefail un error de pg_dump aborta aquí.
pg_dump "${BACKUP_DATABASE_URL}" \
  --no-owner --no-privileges \
  --format=plain \
  | gzip -9 > "${DUMP_FILE}"

# Verificación antes de subir: gzip íntegro, dump terminado y con datos.
gzip -t "${DUMP_FILE}" || fail "El archivo comprimido está corrupto."
TAIL=$(gzip -dc "${DUMP_FILE}" | tail -n 5)
case "${TAIL}" in
  *"PostgreSQL database dump complete"*) ;;
  *) fail "El dump quedó incompleto (no termina con 'dump complete')." ;;
esac
GYMS_COPY=$(gzip -dc "${DUMP_FILE}" | grep -c '^COPY public\.gyms ' || true)
[ "${GYMS_COPY}" = "1" ] || fail "El dump no trae la tabla gyms."

SIZE=$(du -h "${DUMP_FILE}" | cut -f1)
log "dump verificado: ${SIZE}"

if [ -n "${BACKUP_KEEP_DIR:-}" ]; then
  cp "${DUMP_FILE}" "${BACKUP_KEEP_DIR}/"
  log "copia local: ${BACKUP_KEEP_DIR}/$(basename "${DUMP_FILE}")"
fi

if [ "${DRY_RUN}" = "1" ]; then
  log "✓ BACKUP_DRY_RUN=1: no se subió a R2."
  exit 0
fi

# Subida a R2. Path: <bucket>/postgres/YYYY/MM/tinta-<ts>.sql.gz
YEAR=$(date -u +%Y)
MONTH=$(date -u +%m)
S3_KEY="postgres/${YEAR}/${MONTH}/$(basename "${DUMP_FILE}")"

log "→ s3 cp s3://${R2_BUCKET}/${S3_KEY}"
aws s3 cp "${DUMP_FILE}" "s3://${R2_BUCKET}/${S3_KEY}" \
  "${AWS_ARGS[@]}" \
  --only-show-errors

# Confirmar que el objeto quedó en el bucket, no solo que el comando salió.
LISTED=$(aws s3 ls "s3://${R2_BUCKET}/${S3_KEY}" "${AWS_ARGS[@]}" || true)
[ -n "${LISTED}" ] || fail "La subida no aparece en s3://${R2_BUCKET}/${S3_KEY}."
log "✓ backup en R2: ${LISTED}"

# La rotación (delete dailies > 7 días, etc) se configura en CF como
# "Lifecycle rule" del bucket — no hay que hacer nada acá.
