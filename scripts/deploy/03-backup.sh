#!/usr/bin/env bash
# 03-backup.sh — pg_dump + upload a Cloudflare R2 (S3-compatible).
#
# Vive en /opt/tinta/bin/backup.sh en el servidor y se corre cada noche
# vía cron del usuario `tinta`. Conserva 7 dailies + 4 weeklies + 12
# monthlies (rotation hecha en R2 con lifecycle rules).
#
# Setup en el servidor (una sola vez, como root):
#
#   1. apt-get -y install awscli postgresql-client-16
#
#   2. Crear bucket en https://dash.cloudflare.com/?to=/:account/r2 — name:
#      tinta-backups (private). Anota el Account ID.
#
#   3. Crear API token: R2 → Manage R2 API Tokens → "Object Read & Write"
#      scoped al bucket. Te da access_key + secret + endpoint
#      https://<acct>.r2.cloudflarestorage.com.
#
#   4. Como `tinta` user:
#        sudo -u tinta mkdir -p /home/tinta/.aws
#        sudo -u tinta tee /home/tinta/.aws/credentials >/dev/null <<E
#        [r2]
#        aws_access_key_id=...
#        aws_secret_access_key=...
#        E
#        sudo chmod 600 /home/tinta/.aws/credentials
#        sudo chown -R tinta:tinta /home/tinta/.aws
#
#   5. Subir este script:
#        scp 03-backup.sh tinta@$SERVER:/opt/tinta/bin/backup.sh
#        ssh tinta@$SERVER "chmod +x /opt/tinta/bin/backup.sh"
#
#   6. Cron entry (como tinta):
#        crontab -e
#        # Daily at 03:15 UTC (= 21:15 CDMX, después del cierre del gym).
#        15 3 * * * /opt/tinta/bin/backup.sh >> /var/log/tinta/backup.log 2>&1
#
# Variables que el script espera leer del .env del servidor:
#   DATABASE_URL   — para pg_dump.
#   R2_BUCKET      — name del bucket (default tinta-backups).
#   R2_ENDPOINT    — https://<acct>.r2.cloudflarestorage.com
#   R2_PROFILE     — profile name en ~/.aws/credentials (default r2).

set -euo pipefail

ENV_FILE="${ENV_FILE:-/opt/tinta/tinta-server.env}"
if [ -f "${ENV_FILE}" ]; then
  # shellcheck disable=SC1090
  set -a; source "${ENV_FILE}"; set +a
fi

: "${DATABASE_URL:?Falta DATABASE_URL — revisar ${ENV_FILE}}"
: "${R2_ENDPOINT:?Falta R2_ENDPOINT (ej: https://abc123.r2.cloudflarestorage.com)}"
R2_BUCKET="${R2_BUCKET:-tinta-backups}"
R2_PROFILE="${R2_PROFILE:-r2}"

DATE_UTC=$(date -u +%Y-%m-%dT%H-%M-%SZ)
TMPDIR=$(mktemp -d)
trap 'rm -rf "${TMPDIR}"' EXIT

DUMP_FILE="${TMPDIR}/tinta-${DATE_UTC}.sql.gz"

echo "[$(date -u +%FT%TZ)] → pg_dump → ${DUMP_FILE}"
# --no-owner --no-privileges para que el restore sea portable. -F p (plain)
# para gz; alternativamente -F c (custom) si prefieres pg_restore.
pg_dump "${DATABASE_URL}" \
  --no-owner --no-privileges \
  --format=plain \
  | gzip -9 > "${DUMP_FILE}"

SIZE=$(du -h "${DUMP_FILE}" | cut -f1)
echo "[$(date -u +%FT%TZ)] dump size: ${SIZE}"

# Subida a R2. Path: <bucket>/postgres/YYYY/MM/tinta-<ts>.sql.gz
YEAR=$(date -u +%Y)
MONTH=$(date -u +%m)
S3_KEY="postgres/${YEAR}/${MONTH}/$(basename "${DUMP_FILE}")"

echo "[$(date -u +%FT%TZ)] → s3 cp s3://${R2_BUCKET}/${S3_KEY}"
aws s3 cp "${DUMP_FILE}" "s3://${R2_BUCKET}/${S3_KEY}" \
  --endpoint-url "${R2_ENDPOINT}" \
  --profile "${R2_PROFILE}" \
  --only-show-errors

echo "[$(date -u +%FT%TZ)] ✓ backup uploaded"

# Health-check: lista las últimas 5 keys del prefix de hoy para verificar.
echo "[$(date -u +%FT%TZ)] últimos backups en R2:"
aws s3 ls "s3://${R2_BUCKET}/postgres/${YEAR}/${MONTH}/" \
  --endpoint-url "${R2_ENDPOINT}" \
  --profile "${R2_PROFILE}" \
  | tail -5

# La rotación (delete dailies > 7 días, etc) se configura en CF como
# "Lifecycle rule" del bucket — no hay que hacer nada acá.
